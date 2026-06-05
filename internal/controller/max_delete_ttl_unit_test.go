package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newUnitDeployment(name string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("apps/v1")
	obj.SetKind("Deployment")
	obj.SetName(name)
	obj.SetNamespace("default")
	obj.SetAnnotations(annotations)
	return obj
}

func newUnitReconciler(t *testing.T, obj *unstructured.Unstructured) (*LifecycleReconciler, *record.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}

	recorder := record.NewFakeRecorder(10)
	return &LifecycleReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build(),
		Recorder:     recorder,
		MaxDeleteTTL: 30 * 24 * time.Hour,
	}, recorder
}

func expectDeleteTTLExceeded(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "DeleteTTLExceeded") {
			t.Fatalf("expected DeleteTTLExceeded event, got %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected DeleteTTLExceeded event")
	}
}

func fetchUnitDeployment(t *testing.T, c client.Client, key client.ObjectKey) *unstructured.Unstructured {
	t.Helper()

	fetched := &unstructured.Unstructured{}
	fetched.SetAPIVersion("apps/v1")
	fetched.SetKind("Deployment")
	if err := c.Get(context.Background(), key, fetched); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return fetched
}

func TestHandleDeletionRejectsDeleteAfterAboveMaxTTL(t *testing.T) {
	obj := newUnitDeployment("max-ttl-delete-after-unit", map[string]string{DeleteAfterAnnotation: "31d"})
	reconciler, recorder := newUnitReconciler(t, obj)

	result, err := reconciler.handleDeletion(context.Background(), obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle deletion: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %#v", result)
	}

	fetched := fetchUnitDeployment(t, reconciler.Client, client.ObjectKeyFromObject(obj))
	if got := fetched.GetAnnotations()[DeleteAfterAnnotation]; got != "31d" {
		t.Fatalf("expected delete-after to remain 31d, got %q", got)
	}
	if _, ok := fetched.GetAnnotations()[DeleteAtAnnotation]; ok {
		t.Fatal("expected delete-at to remain unset")
	}
	expectDeleteTTLExceeded(t, recorder)
}

func TestHandleDeletionConvertsDeleteAfterAtMaxTTL(t *testing.T) {
	obj := newUnitDeployment("max-ttl-delete-after-allowed-unit", map[string]string{DeleteAfterAnnotation: "30d"})
	reconciler, _ := newUnitReconciler(t, obj)

	result, err := reconciler.handleDeletion(context.Background(), obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle deletion: %v", err)
	}
	if result == (ctrl.Result{}) || result.RequeueAfter != 0 {
		t.Fatalf("expected immediate requeue after conversion, got %#v", result)
	}

	fetched := fetchUnitDeployment(t, reconciler.Client, client.ObjectKeyFromObject(obj))
	annotations := fetched.GetAnnotations()
	if _, ok := annotations[DeleteAfterAnnotation]; ok {
		t.Fatal("expected delete-after to be removed")
	}
	deleteAt, ok := annotations[DeleteAtAnnotation]
	if !ok {
		t.Fatal("expected delete-at to be set")
	}
	if _, err := time.Parse(time.RFC3339, deleteAt); err != nil {
		t.Fatalf("parse delete-at: %v", err)
	}
}

func TestHandleDeletionRejectsFutureDeleteAtAboveMaxTTL(t *testing.T) {
	deleteAt := time.Now().UTC().Add(31 * 24 * time.Hour).Format(time.RFC3339)
	obj := newUnitDeployment("max-ttl-delete-at-unit", map[string]string{DeleteAtAnnotation: deleteAt})
	reconciler, recorder := newUnitReconciler(t, obj)

	result, err := reconciler.handleDeletion(context.Background(), obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle deletion: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %#v", result)
	}

	fetched := fetchUnitDeployment(t, reconciler.Client, client.ObjectKeyFromObject(obj))
	if got := fetched.GetAnnotations()[DeleteAtAnnotation]; got != deleteAt {
		t.Fatalf("expected delete-at to remain %q, got %q", deleteAt, got)
	}
	expectDeleteTTLExceeded(t, recorder)
}

func TestHandleDeletionDeletesPastDeleteAtWithMaxTTL(t *testing.T) {
	obj := newUnitDeployment("max-ttl-delete-at-past-unit", map[string]string{
		DeleteAtAnnotation: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
	})
	reconciler, _ := newUnitReconciler(t, obj)

	result, err := reconciler.handleDeletion(context.Background(), obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle deletion: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %#v", result)
	}

	fetched := &unstructured.Unstructured{}
	fetched.SetAPIVersion("apps/v1")
	fetched.SetKind("Deployment")
	err = reconciler.Get(context.Background(), client.ObjectKeyFromObject(obj), fetched)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected deployment to be deleted, got err=%v", err)
	}
}
