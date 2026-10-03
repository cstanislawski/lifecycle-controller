package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestDeletionPreconditionsEnvtest(t *testing.T) {
	useExistingCluster := false
	environment := &envtest.Environment{UseExistingCluster: &useExistingCluster}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop test API server: %v", err)
		}
	})
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start test API server: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core API: %v", err)
	}
	apiClient, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create test client: %v", err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "deletion-preconditions"}}
	if err := apiClient.Create(t.Context(), namespace); err != nil {
		t.Fatalf("create test namespace: %v", err)
	}

	tests := []struct {
		name         string
		change       func(context.Context, client.Client, *unstructured.Unstructured) error
		wantConflict bool
		wantDeleted  bool
	}{
		{name: "unchanged", wantDeleted: true},
		{
			name: "replacement",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				replacement := obj.DeepCopy()
				replacement.SetUID("")
				replacement.SetResourceVersion("")
				replacement.SetCreationTimestamp(metav1.Time{})
				replacement.SetManagedFields(nil)
				replacement.SetAnnotations(nil)
				return c.Create(ctx, replacement)
			},
			wantConflict: true,
		},
		{
			name: "deadline-removed",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				obj.SetAnnotations(nil)
				return c.Update(ctx, obj)
			},
			wantConflict: true,
		},
		{
			name: "deadline-postponed",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				obj.SetAnnotations(map[string]string{DeleteAtAnnotation: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
				return c.Update(ctx, obj)
			},
			wantConflict: true,
		},
		{
			name: "dry-run-enabled",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				annotations := obj.GetAnnotations()
				annotations[DryRunAnnotation] = "true"
				obj.SetAnnotations(annotations)
				return c.Update(ctx, obj)
			},
			wantConflict: true,
		},
		{
			name: "unrelated-update",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				obj.SetLabels(map[string]string{"changed": "true"})
				return c.Update(ctx, obj)
			},
			wantConflict: true,
			wantDeleted:  true,
		},
		{
			name: "already-deleted",
			change: func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
				return c.Delete(ctx, obj)
			},
			wantDeleted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
			obj.SetName(tt.name)
			obj.SetNamespace(namespace.Name)
			obj.SetAnnotations(map[string]string{DeleteAtAnnotation: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
			if err := apiClient.Create(t.Context(), obj); err != nil {
				t.Fatalf("create target: %v", err)
			}

			deleteCalls := 0
			guardedClient := interceptor.NewClient(apiClient, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, observed client.Object, opts ...client.DeleteOption) error {
					deleteCalls++
					assertDeletionPreconditions(t, observed, opts...)
					if deleteCalls == 1 && tt.change != nil {
						if err := tt.change(ctx, c, observed.(*unstructured.Unstructured).DeepCopy()); err != nil {
							t.Fatalf("change target before deletion: %v", err)
						}
					}
					return c.Delete(ctx, observed, opts...)
				},
			})
			r := &LifecycleReconciler{Client: guardedClient, Recorder: record.NewFakeRecorder(8)}
			request := resourceRequest{NamespacedName: client.ObjectKeyFromObject(obj), GVK: obj.GroupVersionKind()}
			result, err := r.Reconcile(t.Context(), request)
			if tt.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("stale deletion error = %v, want conflict", err)
				}
			} else if err != nil {
				t.Fatalf("delete target: %v", err)
			}
			if !result.IsZero() || deleteCalls != 1 {
				t.Fatalf("first reconcile: result=%+v delete calls=%d", result, deleteCalls)
			}
			current := obj.DeepCopy()
			if tt.wantConflict {
				if err := apiClient.Get(t.Context(), request.NamespacedName, current); err != nil {
					t.Fatalf("read target after conflict: %v", err)
				}
				if tt.name == "replacement" && current.GetUID() == obj.GetUID() {
					t.Fatal("replacement must have a different UID")
				}
			}
			if _, err := r.Reconcile(t.Context(), request); err != nil {
				t.Fatalf("reconcile current target: %v", err)
			}
			final := obj.DeepCopy()
			err = apiClient.Get(t.Context(), request.NamespacedName, final)
			if tt.wantDeleted {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("target must be deleted, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("target must survive, got %v", err)
				}
				if final.GetUID() != current.GetUID() || final.GetResourceVersion() != current.GetResourceVersion() {
					t.Fatal("retry changed the current target")
				}
			}
			wantCalls := 1
			if tt.name == "unrelated-update" {
				wantCalls = 2
			}
			if deleteCalls != wantCalls {
				t.Fatalf("delete calls = %d, want %d", deleteCalls, wantCalls)
			}
		})
	}
}

func assertDeletionPreconditions(t *testing.T, observed client.Object, opts ...client.DeleteOption) {
	t.Helper()
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil ||
		*options.Preconditions.UID != observed.GetUID() {
		t.Error("delete must require the observed UID")
	}
	if options.Preconditions == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.ResourceVersion != observed.GetResourceVersion() {
		t.Error("delete must require the observed resource version")
	}
}
