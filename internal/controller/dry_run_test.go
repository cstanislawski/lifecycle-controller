package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type mutationTrackingClient struct {
	client.Client
	deletes int
	patches int
	updates int
}

func (c *mutationTrackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deletes++
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *mutationTrackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *mutationTrackingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, obj, opts...)
}

func reconcileForDryRunTest(t *testing.T, obj *unstructured.Unstructured, globalDryRun bool) (ctrl.Result, *mutationTrackingClient, *record.FakeRecorder) {
	t.Helper()

	trackedClient := &mutationTrackingClient{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
	}
	recorder := record.NewFakeRecorder(8)
	reconciler := &LifecycleReconciler{
		Client:       trackedClient,
		Recorder:     recorder,
		GlobalDryRun: globalDryRun,
	}

	result, err := reconciler.reconcileLogic(context.Background(), obj, logr.Discard())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result, trackedClient, recorder
}

func assertNoTargetMutation(t *testing.T, before, after *unstructured.Unstructured, trackedClient *mutationTrackingClient) {
	t.Helper()

	if trackedClient.deletes != 0 || trackedClient.patches != 0 || trackedClient.updates != 0 {
		t.Fatalf("Kubernetes mutations: deletes=%d patches=%d updates=%d, want zero", trackedClient.deletes, trackedClient.patches, trackedClient.updates)
	}
	if !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatalf("in-memory target object mutated\nbefore: %#v\nafter:  %#v", before.Object, after.Object)
	}
}

func TestDryRunBooleanSpellingsControlDeletion(t *testing.T) {
	trueValues := []string{"1", "t", "T", "TRUE", "true", "True"}
	falseValues := []string{"0", "f", "F", "FALSE", "false", "False"}

	for _, value := range trueValues {
		t.Run("true_"+value, func(t *testing.T) {
			obj := testDeployment("dry-run-true", map[string]string{
				DeleteAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				DryRunAnnotation:   value,
			})
			before := obj.DeepCopy()

			result, trackedClient, _ := reconcileForDryRunTest(t, obj, false)

			if !result.IsZero() {
				t.Fatalf("result = %#v, want zero", result)
			}
			assertNoTargetMutation(t, before, obj, trackedClient)
		})
	}

	for _, value := range falseValues {
		t.Run("false_"+value, func(t *testing.T) {
			obj := testDeployment("dry-run-false", map[string]string{
				DeleteAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				DryRunAnnotation:   value,
			})

			_, trackedClient, _ := reconcileForDryRunTest(t, obj, false)

			if trackedClient.deletes != 1 {
				t.Fatalf("delete calls = %d, want 1", trackedClient.deletes)
			}
		})
	}
}

func TestGlobalDryRunOverridesResourceFalse(t *testing.T) {
	obj := testDeployment("global-dry-run", map[string]string{
		DeleteAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		DryRunAnnotation:   "false",
	})
	before := obj.DeepCopy()

	result, trackedClient, _ := reconcileForDryRunTest(t, obj, true)

	if !result.IsZero() {
		t.Fatalf("result = %#v, want zero", result)
	}
	assertNoTargetMutation(t, before, obj, trackedClient)
}

func TestInvalidDryRunFailsClosedWithWarningEvent(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		podSpawner  bool
	}{
		{
			name: "delete",
			annotations: map[string]string{
				DeleteAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				DryRunAnnotation:   "not-a-boolean",
			},
		},
		{
			name: "restart",
			annotations: map[string]string{
				RestartAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
				DryRunAnnotation:    "not-a-boolean",
			},
			podSpawner: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj := testDeployment("invalid-dry-run-"+test.name, test.annotations)
			if test.podSpawner {
				setTemplate(t, obj)
			}
			before := obj.DeepCopy()

			result, trackedClient, recorder := reconcileForDryRunTest(t, obj, false)

			if !result.IsZero() {
				t.Fatalf("result = %#v, want zero", result)
			}
			assertNoTargetMutation(t, before, obj, trackedClient)
			select {
			case event := <-recorder.Events:
				if !strings.Contains(event, "InvalidAnnotationValue") || !strings.Contains(event, "taking no action") {
					t.Fatalf("event = %q, want InvalidAnnotationValue taking-no-action warning", event)
				}
			default:
				t.Fatal("missing invalid dry-run warning event")
			}
		})
	}
}

func TestDryRunScheduleModesDoNotMutateOrSelfRequeue(t *testing.T) {
	overdue := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	tests := []struct {
		name        string
		annotations map[string]string
		podSpawner  bool
		eventReason string
	}{
		{
			name: "delete-after",
			annotations: map[string]string{
				DeleteAfterAnnotation: "1h",
				DryRunAnnotation:      "true",
			},
			eventReason: "DryRunDelete",
		},
		{
			name: "restart-after",
			annotations: map[string]string{
				RestartAfterAnnotation: "1h",
				DryRunAnnotation:       "true",
			},
			podSpawner:  true,
			eventReason: "DryRunRestart",
		},
		{
			name: "restart-at-overdue",
			annotations: map[string]string{
				RestartAtAnnotation: overdue,
				DryRunAnnotation:    "true",
			},
			podSpawner:  true,
			eventReason: "DryRunRestart",
		},
		{
			name: "restart-every-initialization",
			annotations: map[string]string{
				RestartEveryAnnotation: "1h",
				DryRunAnnotation:       "true",
			},
			podSpawner: true,
		},
		{
			name: "restart-every-overdue",
			annotations: map[string]string{
				RestartEveryAnnotation: "1h",
				LastRestartTimestamp:   overdue,
				DryRunAnnotation:       "true",
			},
			podSpawner:  true,
			eventReason: "DryRunRestart",
		},
		{
			name: "restart-cron-initialization",
			annotations: map[string]string{
				RestartCronAnnotation: "* * * * *",
				DryRunAnnotation:      "true",
			},
			podSpawner: true,
		},
		{
			name: "restart-cron-overdue",
			annotations: map[string]string{
				RestartCronAnnotation: "* * * * *",
				LastRestartTimestamp:  overdue,
				DryRunAnnotation:      "true",
			},
			podSpawner:  true,
			eventReason: "DryRunRestart",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj := testDeployment("dry-run-"+test.name, test.annotations)
			if test.podSpawner {
				setTemplate(t, obj)
			}
			before := obj.DeepCopy()

			result, trackedClient, recorder := reconcileForDryRunTest(t, obj, false)

			if !result.IsZero() {
				t.Fatalf("result = %#v, want zero to avoid self-requeue", result)
			}
			assertNoTargetMutation(t, before, obj, trackedClient)
			if test.eventReason != "" {
				select {
				case event := <-recorder.Events:
					if !strings.Contains(event, test.eventReason) {
						t.Fatalf("event = %q, want reason %s", event, test.eventReason)
					}
				default:
					t.Fatalf("missing %s planning event", test.eventReason)
				}
			}
			select {
			case event := <-recorder.Events:
				t.Fatalf("unexpected additional event: %q", event)
			default:
			}
		})
	}
}
