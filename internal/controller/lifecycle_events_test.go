package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type forbiddenDeleteClient struct {
	client.Client
}

func (c forbiddenDeleteClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("delete denied"))
}

type forbiddenUpdateClient struct {
	client.Client
}

func (c forbiddenUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), errors.New("update denied"))
}

func TestHandleDeletionEvents(t *testing.T) {
	t.Run("successful action records Deleted", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-successful-delete", map[string]string{
			DeleteAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{
			Client:   eventFakeClient(t, obj),
			Recorder: recorder,
		}

		result, err := reconciler.handleDeletion(ctx, obj, false, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(result).To(gomega.Equal(ctrl.Result{}))

		expectRecorderEvent(t, recorder, "Normal", "Deleted", "Resource deleted based on delete-at annotation")
	})

	t.Run("skipped dry-run records DryRunDelete and keeps target", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-dry-run-delete", map[string]string{
			DeleteAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
			DryRunAnnotation:   "true",
		})
		fakeClient := eventFakeClient(t, obj)
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{Client: fakeClient, Recorder: recorder}

		_, err := reconciler.handleDeletion(ctx, obj, true, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())

		expectRecorderEvent(t, recorder, "Normal", "DryRunDelete", "Dry-run: Resource would be deleted now.")
		fetched := &unstructured.Unstructured{}
		fetched.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
		g.Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(obj), fetched)).To(gomega.Succeed())
	})

	t.Run("invalid timestamp records one InvalidAnnotation", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-invalid-delete-at", map[string]string{
			DeleteAtAnnotation: "not-a-rfc3339-timestamp",
		})
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{
			Client:   eventFakeClient(t, obj),
			Recorder: recorder,
		}

		_, err := reconciler.handleDeletion(ctx, obj, false, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())

		expectRecorderEvent(t, recorder, "Warning", "InvalidAnnotation", "Invalid format for delete-at annotation")
		expectNoRecorderEvent(t, recorder)
	})

	t.Run("forbidden target records DeletionFailed", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-forbidden-delete", map[string]string{
			DeleteAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
		recorder := record.NewFakeRecorder(1)
		baseClient := eventFakeClient(t, obj)
		reconciler := &LifecycleReconciler{
			Client:   forbiddenDeleteClient{Client: baseClient},
			Recorder: recorder,
		}

		_, err := reconciler.handleDeletion(ctx, obj, false, logr.Discard())
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue())

		expectRecorderEvent(t, recorder, "Warning", "DeletionFailed", "Failed to delete resource")
	})
}

func TestHandleRestartEvents(t *testing.T) {
	t.Run("successful action records RestartTriggered", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-successful-restart", map[string]string{
			RestartAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{
			Client:   eventFakeClient(t, obj),
			Recorder: recorder,
		}

		result, err := reconciler.handleRestart(ctx, obj, false, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(result).To(gomega.Equal(ctrl.Result{}))

		expectRecorderEvent(t, recorder, "Normal", "RestartTriggered", "Triggered a rolling restart")
		templateAnnotations, found, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(found).To(gomega.BeTrue())
		g.Expect(templateAnnotations).To(gomega.HaveKey(RestartedAtTemplate))
	})

	t.Run("skipped dry-run records DryRunRestart and keeps target annotation", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-dry-run-restart", map[string]string{
			RestartAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
			DryRunAnnotation:    "true",
		})
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{
			Client:   eventFakeClient(t, obj),
			Recorder: recorder,
		}

		_, err := reconciler.handleRestart(ctx, obj, true, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())

		expectRecorderEvent(t, recorder, "Normal", "DryRunRestart", "Dry-run: Resource would be restarted now.")
		g.Expect(obj.GetAnnotations()).To(gomega.HaveKey(RestartAtAnnotation))
	})

	t.Run("invalid timestamp records one InvalidAnnotation", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-invalid-restart-at", map[string]string{
			RestartAtAnnotation: "not-a-rfc3339-timestamp",
		})
		recorder := record.NewFakeRecorder(1)
		reconciler := &LifecycleReconciler{
			Client:   eventFakeClient(t, obj),
			Recorder: recorder,
		}

		_, err := reconciler.handleRestart(ctx, obj, false, logr.Discard())
		g.Expect(err).NotTo(gomega.HaveOccurred())

		expectRecorderEvent(t, recorder, "Warning", "InvalidAnnotation", "Invalid format for restart-at annotation")
		expectNoRecorderEvent(t, recorder)
	})

	t.Run("forbidden target records RestartFailed", func(t *testing.T) {
		g := gomega.NewWithT(t)
		ctx := context.Background()
		obj := eventDeploymentObject(t, "event-forbidden-restart", map[string]string{
			RestartAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
		})
		recorder := record.NewFakeRecorder(1)
		baseClient := eventFakeClient(t, obj)
		reconciler := &LifecycleReconciler{
			Client:   forbiddenUpdateClient{Client: baseClient},
			Recorder: recorder,
		}

		_, err := reconciler.handleRestart(ctx, obj, false, logr.Discard())
		g.Expect(err).To(gomega.HaveOccurred())
		g.Expect(apierrors.IsForbidden(err)).To(gomega.BeTrue())

		expectRecorderEvent(t, recorder, "Warning", "RestartFailed", "Could not update object to trigger restart")
	})
}

func eventDeploymentObject(t *testing.T, name string, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workload", Image: "nginx"}}},
			},
		},
	}
	objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
	if err != nil {
		t.Fatalf("convert deployment to unstructured: %v", err)
	}
	return &unstructured.Unstructured{Object: objectMap}
}

func eventFakeClient(t *testing.T, obj client.Object) client.Client {
	t.Helper()
	g := gomega.NewWithT(t)
	g.Expect(appsv1.AddToScheme(scheme.Scheme)).To(gomega.Succeed())
	g.Expect(corev1.AddToScheme(scheme.Scheme)).To(gomega.Succeed())
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(obj).Build()
}

func expectRecorderEvent(t *testing.T, recorder *record.FakeRecorder, eventType, reason string, messageParts ...string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		expectedPrefix := fmt.Sprintf("%s %s", eventType, reason)
		if !strings.Contains(event, expectedPrefix) {
			t.Fatalf("expected event to contain %q, got %q", expectedPrefix, event)
		}
		for _, part := range messageParts {
			if !strings.Contains(event, part) {
				t.Fatalf("expected event to contain %q, got %q", part, event)
			}
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s %s event", eventType, reason)
	}
}

func expectNoRecorderEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("expected no duplicate event, got %q", event)
	case <-time.After(100 * time.Millisecond):
	}
}
