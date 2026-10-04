package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var catchUpTestStart = time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)

func TestCatchUpNeverUnusedConfigurationKeepsSelectedArm(t *testing.T) {
	for _, tt := range []struct {
		action, modifier, value string
	}{
		{DeleteAtAnnotation, ReferencePointAnnotation, ReferencePointCreationTimestamp},
		{RestartAtAnnotation, ReferencePointAnnotation, ReferencePointCreationTimestamp},
		{RestartAtAnnotation, CronTimezoneAnnotation, "Europe/Warsaw"},
		{RestartAtAnnotation, RestartCronAnnotation, "0 3 * * *"},
		{RestartAtAnnotation, RestartEveryAnnotation, "1h"},
		{RestartAtAnnotation, LastRestartTimestamp, catchUpTestStart.Format(time.RFC3339)},
	} {
		t.Run(tt.action+" "+tt.modifier, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("selected-arm", map[string]string{tt.action: due.Format(time.RFC3339)})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, r, obj)
			catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
				a := current.GetAnnotations()
				a[tt.modifier] = tt.value
				current.SetAnnotations(a)
			})
			now = due.Add(time.Second)
			catchUpReconcile(t, r, obj)
			catchUpAssertExecuted(t, r, obj, tt.action)
		})
	}
}

func catchUpNeverReconciler(t *testing.T, obj *unstructured.Unstructured, now *time.Time, funcs interceptor.Funcs) *LifecycleReconciler {
	t.Helper()
	policy, err := ParseCatchUpPolicy("never")
	if err != nil {
		t.Fatalf("parse never policy: %v", err)
	}
	return &LifecycleReconciler{
		Client:       fake.NewClientBuilder().WithObjects(obj.DeepCopy()).WithInterceptorFuncs(funcs).Build(),
		Recorder:     record.NewFakeRecorder(64),
		ActionTiming: ActionTimingConfig{Delete: policy, Restart: policy},
		now:          func() time.Time { return *now },
	}
}

func catchUpReconcile(t *testing.T, r *LifecycleReconciler, obj *unstructured.Unstructured) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), resourceRequest{
		NamespacedName: client.ObjectKeyFromObject(obj),
		GVK:            obj.GroupVersionKind(),
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func catchUpFetch(t *testing.T, r *LifecycleReconciler, obj *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	return getObject(t, r, client.ObjectKeyFromObject(obj))
}

func catchUpUpdate(t *testing.T, r *LifecycleReconciler, obj *unstructured.Unstructured, change func(*unstructured.Unstructured)) {
	t.Helper()
	current := catchUpFetch(t, r, obj)
	change(current)
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatalf("update resource: %v", err)
	}
}

func catchUpAssertSkipped(t *testing.T, r *LifecycleReconciler, obj *unstructured.Unstructured, action string) {
	t.Helper()
	current := catchUpFetch(t, r, obj)
	if _, exists := current.GetAnnotations()[action]; exists {
		t.Fatalf("skipped request still present: %s", action)
	}
	if got := restartedAt(t, current); got != "" {
		t.Fatalf("skipped request changed template: %q", got)
	}
}

func catchUpAssertExecuted(t *testing.T, r *LifecycleReconciler, obj *unstructured.Unstructured, action string) {
	t.Helper()
	if action == RestartAtAnnotation {
		assertOneTimeRestartAcknowledged(t, catchUpFetch(t, r, obj))
		return
	}
	current := obj.DeepCopy()
	err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), current)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get deleted resource: %v, want NotFound", err)
	}
}

func TestCatchUpNeverArmedOneTimeActionsExecute(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation} {
		for _, source := range []string{"global", "annotation"} {
			t.Run(action+"/"+source, func(t *testing.T) {
				now := catchUpTestStart
				due := now.Add(time.Minute)
				obj := testDeployment("armed-action", map[string]string{action: due.Format(time.RFC3339)})
				setTemplate(t, obj)
				r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
				if source == "annotation" {
					r.ActionTiming = ActionTimingConfig{}
					catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
						annotations := current.GetAnnotations()
						annotations[CatchUpAnnotation] = "never"
						current.SetAnnotations(annotations)
					})
				}
				catchUpReconcile(t, r, obj)
				now = due.Add(250 * time.Millisecond)
				catchUpReconcile(t, r, obj)
				catchUpAssertExecuted(t, r, obj, action)
			})
		}
	}
}

func TestCatchUpNeverRecoverySkipsOneTimeActions(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("recovered-action", map[string]string{action: due.Format(time.RFC3339)})
			setTemplate(t, obj)
			original := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, original, obj)
			now = due.Add(time.Second)
			recovered := catchUpNeverReconciler(t, catchUpFetch(t, original, obj), &now, interceptor.Funcs{})
			catchUpReconcile(t, recovered, obj)
			catchUpAssertSkipped(t, recovered, obj, action)
		})
	}
}

func TestCatchUpNeverAPIFailureRetainsArmedAction(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("retry-action", map[string]string{action: due.Format(time.RFC3339)})
			setTemplate(t, obj)
			failure := errors.New("temporary API failure")
			fail := true
			calls := 0
			funcs := interceptor.Funcs{}
			if action == DeleteAtAnnotation {
				funcs.Delete = func(ctx context.Context, c client.WithWatch, target client.Object, opts ...client.DeleteOption) error {
					calls++
					if fail {
						return failure
					}
					return c.Delete(ctx, target, opts...)
				}
			} else {
				funcs.Patch = func(ctx context.Context, c client.WithWatch, target client.Object, patch client.Patch, opts ...client.PatchOption) error {
					calls++
					if fail {
						return failure
					}
					return c.Patch(ctx, target, patch, opts...)
				}
			}
			r := catchUpNeverReconciler(t, obj, &now, funcs)
			catchUpReconcile(t, r, obj)
			now = due.Add(time.Second)
			_, err := r.Reconcile(context.Background(), resourceRequest{NamespacedName: client.ObjectKeyFromObject(obj), GVK: obj.GroupVersionKind()})
			if !errors.Is(err, failure) {
				t.Fatalf("failed action error = %v, want temporary API failure", err)
			}
			fail = false
			now = now.Add(10 * time.Minute)
			catchUpReconcile(t, r, obj)
			catchUpAssertExecuted(t, r, obj, action)
			if calls != 2 {
				t.Fatalf("action calls = %d, want 2", calls)
			}
		})
	}
}

func TestCatchUpNeverChangedIdentityDoesNotReuseArm(t *testing.T) {
	for _, change := range []string{"UID", "deadline", "policy"} {
		t.Run(change, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("changed-action", map[string]string{RestartAtAnnotation: due.Format(time.RFC3339)})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, r, obj)
			catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
				annotations := current.GetAnnotations()
				switch change {
				case "UID":
					current.SetUID("replacement")
				case "deadline":
					annotations[RestartAtAnnotation] = due.Add(-time.Second).Format(time.RFC3339)
				case "policy":
					annotations[CatchUpAnnotation] = "always"
				}
				current.SetAnnotations(annotations)
			})
			if change == "policy" {
				catchUpReconcile(t, r, obj)
				catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
					annotations := current.GetAnnotations()
					annotations[CatchUpAnnotation] = "never"
					current.SetAnnotations(annotations)
				})
			}
			now = due.Add(time.Second)
			catchUpReconcile(t, r, obj)
			catchUpAssertSkipped(t, r, obj, RestartAtAnnotation)
		})
	}
}

func TestCatchUpNeverRelativeConversionArmsActions(t *testing.T) {
	for _, action := range []string{DeleteAfterAnnotation, RestartAfterAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart
			obj := testDeployment("relative-action", map[string]string{action: "1m"})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, r, obj)
			absolute := DeleteAtAnnotation
			if action == RestartAfterAnnotation {
				absolute = RestartAtAnnotation
			}
			converted := catchUpFetch(t, r, obj)
			if got := converted.GetAnnotations()[absolute]; got != now.Add(time.Minute).Format(time.RFC3339) {
				t.Fatalf("converted deadline = %q, want one minute from processing", got)
			}
			now = now.Add(time.Minute + time.Second)
			catchUpReconcile(t, r, obj)
			catchUpAssertExecuted(t, r, obj, absolute)
		})
	}
}

func TestCatchUpNeverRecurringInitializationArmsNextOccurrence(t *testing.T) {
	for _, schedule := range []struct{ annotation, value string }{
		{RestartEveryAnnotation, "1h"},
		{RestartCronAnnotation, "0 * * * *"},
	} {
		t.Run(schedule.annotation, func(t *testing.T) {
			now := catchUpTestStart
			obj := testDeployment("initial-recurring", map[string]string{schedule.annotation: schedule.value})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, r, obj)
			now = now.Add(time.Hour + time.Second)
			catchUpReconcile(t, r, obj)
			assertRestartMarker(t, catchUpFetch(t, r, obj))
		})
	}
}

func TestCatchUpNeverRecurringSkipPreservesCadence(t *testing.T) {
	now := catchUpTestStart.Add(3*time.Hour + 20*time.Minute)
	obj := testDeployment("missed-recurring", map[string]string{
		RestartEveryAnnotation: "1h",
		LastRestartTimestamp:   catchUpTestStart.Format(time.RFC3339),
	})
	setTemplate(t, obj)
	r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
	catchUpReconcile(t, r, obj)
	current := catchUpFetch(t, r, obj)
	wantAnchor := catchUpTestStart.Add(3 * time.Hour).Format(time.RFC3339)
	if got := current.GetAnnotations()[LastRestartTimestamp]; got != wantAnchor {
		t.Fatalf("skip anchor = %q, want %q", got, wantAnchor)
	}
	if got := restartedAt(t, current); got != "" {
		t.Fatalf("skipped recurring occurrence changed template: %q", got)
	}
	if got := current.GetAnnotations()[RestartEveryAnnotation]; got != "1h" {
		t.Fatalf("recurring schedule = %q, want 1h", got)
	}
	now = catchUpTestStart.Add(4*time.Hour + time.Second)
	catchUpReconcile(t, r, obj)
	assertRestartMarker(t, catchUpFetch(t, r, obj))
}

func TestCatchUpNeverChangedRecurringScheduleDoesNotReuseArm(t *testing.T) {
	now := catchUpTestStart
	obj := testDeployment("changed-recurring", map[string]string{
		RestartEveryAnnotation: "1h",
		LastRestartTimestamp:   catchUpTestStart.Format(time.RFC3339),
	})
	setTemplate(t, obj)
	r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
	catchUpReconcile(t, r, obj)
	catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
		annotations := current.GetAnnotations()
		annotations[RestartEveryAnnotation] = "2h"
		current.SetAnnotations(annotations)
	})
	now = catchUpTestStart.Add(2*time.Hour + time.Second)
	catchUpReconcile(t, r, obj)
	if got := restartedAt(t, catchUpFetch(t, r, obj)); got != "" {
		t.Fatalf("changed schedule reused old arm: %q", got)
	}
}

func TestCatchUpNeverDryRunDoesNotWriteOrArm(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("dry-run-action", map[string]string{
				action:           due.Format(time.RFC3339),
				DryRunAnnotation: "true",
			})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			before := catchUpFetch(t, r, obj)
			catchUpReconcile(t, r, obj)
			now = due.Add(time.Second)
			catchUpReconcile(t, r, obj)
			if after := catchUpFetch(t, r, obj); !reflect.DeepEqual(before.Object, after.Object) {
				t.Fatal("dry-run changed resource")
			}
			catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
				annotations := current.GetAnnotations()
				delete(annotations, DryRunAnnotation)
				current.SetAnnotations(annotations)
			})
			catchUpReconcile(t, r, obj)
			catchUpAssertSkipped(t, r, obj, action)
		})
	}
}
