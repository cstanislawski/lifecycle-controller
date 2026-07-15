package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestOneTimeRestartCommittedPatchDoesNotReplayAfterLostResponse(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "one-time-lost-response", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{
		RestartAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	setTemplate(t, obj)

	patchCalls := 0
	updateCalls := 0
	lostResponse := errors.New("response lost after patch committed")
	r := &LifecycleReconciler{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalls++
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				return lostResponse
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateCalls++
				return c.Update(ctx, obj, opts...)
			},
		}).Build(),
		Recorder: record.NewFakeRecorder(8),
	}

	observed := getObject(t, r, key)
	if _, err := r.handleRestart(ctx, observed, false, logr.Discard()); !errors.Is(err, lostResponse) {
		t.Fatalf("handle restart error = %v, want lost response", err)
	}

	committed := getObject(t, r, key)
	assertOneTimeRestartAcknowledged(t, committed)
	if patchCalls != 1 {
		t.Fatalf("patch calls = %d, want 1", patchCalls)
	}
	if updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0", updateCalls)
	}

	if _, err := r.handleRestart(ctx, committed, false, logr.Discard()); err != nil {
		t.Fatalf("retry handle restart: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patch calls after retry = %d, want 1", patchCalls)
	}
}

func TestRecurringRestartCommittedPatchDoesNotReplayAfterLostResponse(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "recurring-lost-response", Namespace: "default"}
	duration := 2 * time.Hour
	schedule := intervalRecurringSchedule{duration: duration}
	lastRestart := time.Now().UTC().Add(-duration - time.Minute).Truncate(time.Second)
	nextRestart := lastRestart.Add(duration)
	obj := testDeployment(key.Name, map[string]string{
		RestartEveryAnnotation: "2h",
		LastRestartTimestamp:   lastRestart.Format(time.RFC3339),
	})
	setTemplate(t, obj)

	patchCalls := 0
	updateCalls := 0
	lostResponse := errors.New("response lost after patch committed")
	r := &LifecycleReconciler{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalls++
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				return lostResponse
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateCalls++
				return c.Update(ctx, obj, opts...)
			},
		}).Build(),
		Recorder: record.NewFakeRecorder(8),
	}

	observed := getObject(t, r, key)
	if _, err := r.reconcileRecurringRestart(ctx, observed, false, "interval", schedule, logr.Discard()); !errors.Is(err, lostResponse) {
		t.Fatalf("reconcile recurring restart error = %v, want lost response", err)
	}

	committed := getObject(t, r, key)
	if got := committed.GetAnnotations()[LastRestartTimestamp]; got != nextRestart.Format(time.RFC3339) {
		t.Fatalf("last restart timestamp = %q, want %q", got, nextRestart.Format(time.RFC3339))
	}
	assertRestartMarker(t, committed)
	if patchCalls != 1 {
		t.Fatalf("patch calls = %d, want 1", patchCalls)
	}
	if updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0", updateCalls)
	}

	if _, err := r.reconcileRecurringRestart(ctx, committed, false, "interval", schedule, logr.Discard()); err != nil {
		t.Fatalf("retry recurring restart: %v", err)
	}
	if patchCalls != 1 {
		t.Fatalf("patch calls after retry = %d, want 1", patchCalls)
	}
}

func TestRestartConflictPreservesConcurrentChangesAndRetriesSafely(t *testing.T) {
	const (
		concurrentAnnotation = "example.com/concurrent"
		concurrentValue      = "preserved"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: "restart-conflict", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{
		RestartAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	setTemplate(t, obj)

	patchCalls := 0
	successfulPatches := 0
	updateCalls := 0
	injectConflict := true
	r := &LifecycleReconciler{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalls++
				if injectConflict {
					injectConflict = false
					concurrent := testDeployment(key.Name, nil)
					if err := c.Get(ctx, key, concurrent); err != nil {
						return err
					}
					annotations := concurrent.GetAnnotations()
					annotations[concurrentAnnotation] = concurrentValue
					concurrent.SetAnnotations(annotations)
					if err := c.Update(ctx, concurrent); err != nil {
						return err
					}
				}

				err := c.Patch(ctx, obj, patch, opts...)
				if err == nil {
					successfulPatches++
				}
				return err
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updateCalls++
				return c.Update(ctx, obj, opts...)
			},
		}).Build(),
		Recorder: record.NewFakeRecorder(8),
	}

	stale := getObject(t, r, key)
	if _, err := r.handleRestart(ctx, stale, false, logr.Discard()); !apierrors.IsConflict(err) {
		t.Fatalf("handle restart error = %v, want conflict", err)
	}

	afterConflict := getObject(t, r, key)
	if got := afterConflict.GetAnnotations()[concurrentAnnotation]; got != concurrentValue {
		t.Fatalf("concurrent annotation = %q, want %q", got, concurrentValue)
	}
	if _, found := afterConflict.GetAnnotations()[RestartAtAnnotation]; !found {
		t.Fatalf("restart-at removed by conflicted patch")
	}
	if marker := restartedAt(t, afterConflict); marker != "" {
		t.Fatalf("restart marker = %q after conflict, want empty", marker)
	}

	if _, err := r.handleRestart(ctx, afterConflict, false, logr.Discard()); err != nil {
		t.Fatalf("retry handle restart: %v", err)
	}
	committed := getObject(t, r, key)
	assertOneTimeRestartAcknowledged(t, committed)
	if got := committed.GetAnnotations()[concurrentAnnotation]; got != concurrentValue {
		t.Fatalf("concurrent annotation after retry = %q, want %q", got, concurrentValue)
	}
	if patchCalls != 2 {
		t.Fatalf("patch calls = %d, want 2", patchCalls)
	}
	if successfulPatches != 1 {
		t.Fatalf("successful patches = %d, want 1", successfulPatches)
	}
	if updateCalls != 0 {
		t.Fatalf("controller update calls = %d, want 0", updateCalls)
	}
}

func assertOneTimeRestartAcknowledged(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	annotations := obj.GetAnnotations()
	if _, found := annotations[RestartAtAnnotation]; found {
		t.Fatalf("restart-at still present")
	}
	if got := annotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("managed-by = %q, want %q", got, ManagedByValue)
	}
	assertRestartMarker(t, obj)
}

func assertRestartMarker(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	templateAnnotations, found, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	if !found {
		t.Fatalf("template annotations missing")
	}
	if got := templateAnnotations[ManagedByAnnotation]; got != ManagedByValue {
		t.Fatalf("template managed-by = %q, want %q", got, ManagedByValue)
	}
	if got := templateAnnotations[RestartedAtTemplate]; got == "" {
		t.Fatalf("restart marker missing")
	}
}

func restartedAt(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	templateAnnotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	return templateAnnotations[RestartedAtTemplate]
}
