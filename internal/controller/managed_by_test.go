package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testDeployment(name string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	obj.SetName(name)
	obj.SetNamespace("default")
	obj.SetAnnotations(annotations)
	return obj
}

func setTemplate(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()

	err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{"app": "managed-by"},
		},
		"spec": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{"name": "main", "image": "nginx"},
			},
		},
	}, "spec", "template")
	if err != nil {
		t.Fatalf("set template: %v", err)
	}
}

func getObject(t *testing.T, r *LifecycleReconciler, key types.NamespacedName) *unstructured.Unstructured {
	t.Helper()

	fetched := testDeployment(key.Name, nil)
	if err := r.Get(context.Background(), key, fetched); err != nil {
		t.Fatalf("get object: %v", err)
	}
	return fetched
}

func TestManagedByAddedForDeleteAfterConversion(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "managed-by-delete-after", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{DeleteAfterAnnotation: "1h"})
	r := &LifecycleReconciler{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}

	if _, err := r.handleDeletion(ctx, obj, false, logr.Discard()); err != nil {
		t.Fatalf("handle deletion: %v", err)
	}

	annotations := getObject(t, r, key).GetAnnotations()
	if got := annotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("managed-by = %q, want %q", got, ManagedByValue)
	}
	if _, found := annotations[DeleteAfterAnnotation]; found {
		t.Fatalf("delete-after still present")
	}
	if _, found := annotations[DeleteAtAnnotation]; !found {
		t.Fatalf("delete-at missing")
	}
}

func TestManagedByRestartAfterConversionStaysTopLevelOnly(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "managed-by-restart-after", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{RestartAfterAnnotation: "1h"})
	setTemplate(t, obj)
	r := &LifecycleReconciler{
		Client:   fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
		Recorder: record.NewFakeRecorder(8),
	}

	if _, err := r.handleRestart(ctx, obj, false, logr.Discard()); err != nil {
		t.Fatalf("handle restart: %v", err)
	}

	fetched := getObject(t, r, key)
	annotations := fetched.GetAnnotations()
	if got := annotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("managed-by = %q, want %q", got, ManagedByValue)
	}
	if _, found := annotations[RestartAtAnnotation]; !found {
		t.Fatalf("restart-at missing")
	}

	templateAnnotations, found, err := unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	if found {
		if got := templateAnnotations[ManagedByAnnotation]; got != "" {
			t.Fatalf("template managed-by = %q, want empty for conversion-only update", got)
		}
	}
}

func TestManagedByRecurringRestartInitializationStaysTopLevelOnly(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "managed-by-recurring-init", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{RestartEveryAnnotation: "1h"})
	setTemplate(t, obj)
	r := &LifecycleReconciler{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
	}

	if _, err := r.reconcileRecurringRestart(ctx, obj, false, "interval", time.Hour, logr.Discard()); err != nil {
		t.Fatalf("reconcile recurring restart: %v", err)
	}

	fetched := getObject(t, r, key)
	annotations := fetched.GetAnnotations()
	if got := annotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("managed-by = %q, want %q", got, ManagedByValue)
	}
	if _, found := annotations[LastRestartTimestamp]; !found {
		t.Fatalf("last restart timestamp missing")
	}

	templateAnnotations, found, err := unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	if found {
		if got := templateAnnotations[ManagedByAnnotation]; got != "" {
			t.Fatalf("template managed-by = %q, want empty for recurring initialization", got)
		}
		if got := templateAnnotations[RestartedAtTemplate]; got != "" {
			t.Fatalf("template restartedAt = %q, want empty for recurring initialization", got)
		}
	}
}

func TestManagedByAddedToTopLevelAndTemplateForRestartTrigger(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "managed-by-restart-trigger", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{
		RestartAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	obj.SetResourceVersion("1")
	setTemplate(t, obj)
	r := &LifecycleReconciler{
		Client:   fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
		Recorder: record.NewFakeRecorder(8),
	}

	if _, err := r.handleRestart(ctx, obj, false, logr.Discard()); err != nil {
		t.Fatalf("handle restart: %v", err)
	}

	fetched := getObject(t, r, key)
	annotations := fetched.GetAnnotations()
	if got := annotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("managed-by = %q, want %q", got, ManagedByValue)
	}
	if _, found := annotations[RestartAtAnnotation]; found {
		t.Fatalf("restart-at still present")
	}

	templateAnnotations, found, err := unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	if !found {
		t.Fatalf("template annotations missing")
	}
	if got := templateAnnotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("template managed-by = %q, want %q", got, ManagedByValue)
	}
	if _, found := templateAnnotations[RestartedAtTemplate]; !found {
		t.Fatalf("restartedAt template annotation missing")
	}
}
