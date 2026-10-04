package controller

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCatchUpPolicyParsing(t *testing.T) {
	for _, value := range []string{"always", "never", "1s", "15m", "2h", "7d"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseCatchUpPolicy(value); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, value := range []string{"", "none", "Always", " NEVER", "0s", "-1m", "+1m", "1.5m", "1h30m", "15 m", "999999999999999999d"} {
		t.Run("invalid "+value, func(t *testing.T) {
			if _, err := ParseCatchUpPolicy(value); err == nil {
				t.Fatalf("accepted invalid catch-up policy %q", value)
			}
		})
	}
}

func TestCatchUpPolicyDefaultsAndOverrides(t *testing.T) {
	never, err := ParseCatchUpPolicy("never")
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := ParseCatchUpPolicy("15m")
	if err != nil {
		t.Fatal(err)
	}
	r := &LifecycleReconciler{ActionTiming: ActionTimingConfig{Delete: never, Restart: deadline}}
	obj := objectWithAnnotations(nil)
	for _, tt := range []struct {
		action string
		want   CatchUpPolicy
	}{{deleteAction, never}, {restartAction, deadline}} {
		got, valid := r.catchUpPolicy(obj, tt.action, false)
		if !valid || got != tt.want {
			t.Fatalf("%s default = %+v, %t; want %+v", tt.action, got, valid, tt.want)
		}
	}
	obj.SetAnnotations(map[string]string{CatchUpAnnotation: "always"})
	for _, action := range []string{deleteAction, restartAction} {
		got, valid := r.catchUpPolicy(obj, action, false)
		if !valid || got != (CatchUpPolicy{}) {
			t.Fatalf("annotation did not override %s default: %+v, %t", action, got, valid)
		}
	}
	got, valid := (&LifecycleReconciler{}).catchUpPolicy(objectWithAnnotations(nil), deleteAction, false)
	if !valid || got != (CatchUpPolicy{}) {
		t.Fatalf("zero config = %+v, %t; want always", got, valid)
	}
}

func TestCatchUpInvalidAnnotationBlocksAllMutations(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation, DeleteAfterAnnotation, RestartEveryAnnotation} {
		for _, value := range []string{"", "none", "0s"} {
			t.Run(action+value, func(t *testing.T) {
				obj := testDeployment("invalid-policy", map[string]string{action: "1h", CatchUpAnnotation: value})
				setTemplate(t, obj)
				tracked := &mutationTrackingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
				recorder := record.NewFakeRecorder(8)
				r := &LifecycleReconciler{Client: tracked, Recorder: recorder}
				before := obj.DeepCopy()
				result, err := r.reconcileLogic(t.Context(), obj, logr.Discard())
				if err != nil || !result.IsZero() {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				assertNoTargetMutation(t, before, obj, tracked)
				if event := <-recorder.Events; !strings.HasPrefix(event, "Warning InvalidAnnotation ") {
					t.Fatalf("event=%q", event)
				}
			})
		}
	}
}

func TestCatchUpDeadlineOneTimeBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, action := range []string{deleteAction, restartAction} {
		for _, tt := range []struct {
			name string
			late time.Duration
			skip bool
		}{{"inside", 14 * time.Minute, false}, {"boundary", 15 * time.Minute, false}, {"expired", 15*time.Minute + time.Nanosecond, true}} {
			t.Run(action+" "+tt.name, func(t *testing.T) {
				annotation := DeleteAtAnnotation
				if action == restartAction {
					annotation = RestartAtAnnotation
				}
				obj := testDeployment("deadline", map[string]string{annotation: now.Add(-tt.late).Format(time.RFC3339Nano), CatchUpAnnotation: "15m"})
				setTemplate(t, obj)
				tracked := &mutationTrackingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
				r := &LifecycleReconciler{Client: tracked, Recorder: record.NewFakeRecorder(8), now: func() time.Time { return now }}
				if _, err := r.reconcileLogic(t.Context(), obj, logr.Discard()); err != nil {
					t.Fatal(err)
				}
				if action == deleteAction && !tt.skip {
					if tracked.deletes != 1 {
						t.Fatalf("deletes=%d", tracked.deletes)
					}
					return
				}
				persisted := getObject(t, r, client.ObjectKeyFromObject(obj))
				if _, found := persisted.GetAnnotations()[annotation]; found {
					t.Fatal("one-time occurrence not acknowledged")
				}
				_, marker, err := unstructured.NestedString(persisted.Object, "spec", "template", "metadata", "annotations", RestartedAtTemplate)
				if err != nil || marker != (action == restartAction && !tt.skip) || tracked.deletes != 0 {
					t.Fatalf("marker=%t deletes=%d err=%v", marker, tracked.deletes, err)
				}
			})
		}
	}
}

func TestCatchUpRecurringWindow(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	cronSchedule, err := cron.ParseStandard("CRON_TZ=UTC 0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	for _, schedule := range []recurringSchedule{intervalRecurringSchedule{duration: 24 * time.Hour}, cronRecurringSchedule{Schedule: cronSchedule}} {
		for _, tt := range []struct {
			name string
			now  time.Time
			want bool
		}{
			{"recent after old misses", anchor.Add(72*time.Hour + 10*time.Minute), true},
			{"inclusive boundary", anchor.Add(72*time.Hour + 15*time.Minute), true},
			{"expired", anchor.Add(72*time.Hour + 15*time.Minute + time.Nanosecond), false},
			{"future anchor", anchor.Add(-time.Minute), false},
			{"already acknowledged", anchor, false},
		} {
			t.Run(reflect.TypeOf(schedule).Name()+" "+tt.name, func(t *testing.T) {
				_, got := occurrenceWithinWindow(schedule, anchor, tt.now, 15*time.Minute)
				if got != tt.want {
					t.Fatalf("eligible=%t, want %t", got, tt.want)
				}
			})
		}
	}
}

func TestCatchUpRecurringWindowTimezoneAndDST(t *testing.T) {
	schedule, err := cron.ParseStandard("CRON_TZ=Europe/Warsaw 30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, now string
		want      bool
	}{
		{"before spring jump", "2026-03-28T01:40:00Z", true},
		{"spring nonexistent occurrence", "2026-03-29T01:40:00Z", false},
		{"after spring jump", "2026-03-30T00:40:00Z", true},
		{"first fall occurrence", "2026-10-25T00:40:00Z", true},
		{"second fall occurrence", "2026-10-25T01:40:00Z", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			_, got := occurrenceWithinWindow(cronRecurringSchedule{Schedule: schedule}, now.Add(-7*24*time.Hour), now, 15*time.Minute)
			if got != tt.want {
				t.Fatalf("eligible=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestCatchUpDeadlineExpiresDuringRetry(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	obj := testDeployment("retry-deadline", map[string]string{RestartAtAnnotation: now.Add(-14 * time.Minute).Format(time.RFC3339), CatchUpAnnotation: "15m"})
	setTemplate(t, obj)
	failure := errors.New("API unavailable")
	calls := 0
	r := &LifecycleReconciler{
		Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, target client.Object, patch client.Patch, opts ...client.PatchOption) error {
				calls++
				if calls == 1 {
					return failure
				}
				return c.Patch(ctx, target, patch, opts...)
			},
		}).Build(),
		Recorder: record.NewFakeRecorder(8), now: func() time.Time { return now },
	}
	if _, err := r.reconcileLogic(t.Context(), obj, logr.Discard()); !errors.Is(err, failure) {
		t.Fatalf("first error=%v", err)
	}
	now = now.Add(2 * time.Minute)
	obj = getObject(t, r, client.ObjectKeyFromObject(obj))
	if _, err := r.reconcileLogic(t.Context(), obj, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	got := getObject(t, r, client.ObjectKeyFromObject(obj))
	if _, found := got.GetAnnotations()[RestartAtAnnotation]; found {
		t.Fatal("expired retry not consumed")
	}
	if _, found, _ := unstructured.NestedString(got.Object, "spec", "template", "metadata", "annotations", RestartedAtTemplate); found {
		t.Fatal("retry restarted after deadline")
	}
}

func TestCatchUpRecurringDeadlineReconcile(t *testing.T) {
	anchor := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for _, action := range []string{RestartCronAnnotation, RestartEveryAnnotation} {
		for _, late := range []time.Duration{10 * time.Minute, 20 * time.Minute} {
			t.Run(action+late.String(), func(t *testing.T) {
				now := anchor.Add(72*time.Hour + late)
				value := "24h"
				if action == RestartCronAnnotation {
					value = "0 3 * * *"
				}
				obj := testDeployment("recurring-deadline", map[string]string{action: value, LastRestartTimestamp: anchor.Format(time.RFC3339), CatchUpAnnotation: "15m"})
				setTemplate(t, obj)
				r := &LifecycleReconciler{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(), Recorder: record.NewFakeRecorder(8), now: func() time.Time { return now }}
				result, err := r.reconcileLogic(t.Context(), obj, logr.Discard())
				if err != nil || result.RequeueAfter <= 0 {
					t.Fatalf("result=%+v error=%v", result, err)
				}
				persisted := getObject(t, r, client.ObjectKeyFromObject(obj))
				if persisted.GetAnnotations()[LastRestartTimestamp] == anchor.Format(time.RFC3339) || persisted.GetAnnotations()[action] != value {
					t.Fatal("recurring state not advanced or schedule removed")
				}
				_, marker, err := unstructured.NestedString(persisted.Object, "spec", "template", "metadata", "annotations", RestartedAtTemplate)
				if err != nil || marker != (late <= 15*time.Minute) {
					t.Fatalf("marker=%t err=%v", marker, err)
				}
				if late > 15*time.Minute {
					before, _, _ := unstructured.NestedMap(obj.Object, "spec", "template")
					after, _, _ := unstructured.NestedMap(persisted.Object, "spec", "template")
					if !reflect.DeepEqual(before, after) {
						t.Fatal("skip changed template")
					}
				}
			})
		}
	}
}

func TestCatchUpDryRunExpiredActionDoesNotAcknowledge(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation, RestartEveryAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := time.Now().UTC()
			value := now.Add(-time.Hour).Format(time.RFC3339)
			if action == RestartEveryAnnotation {
				value = "1h"
			}
			obj := testDeployment("dry-skip", map[string]string{action: value, CatchUpAnnotation: "1s", LastRestartTimestamp: now.Add(-90 * time.Minute).Format(time.RFC3339)})
			setTemplate(t, obj)
			before := obj.DeepCopy()
			_, tracked, recorder := reconcileForDryRunTest(t, obj, true)
			assertNoTargetMutation(t, before, obj, tracked)
			if event := <-recorder.Events; !strings.Contains(event, "DryRunActionSkipped") {
				t.Fatalf("event=%q", event)
			}
		})
	}
}

func TestCatchUpNotFoundAndNoActionForgetArms(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run("missing="+strconv.FormatBool(missing), func(t *testing.T) {
			now := time.Now().UTC()
			obj := testDeployment("removed-action", map[string]string{RestartAtAnnotation: now.Add(time.Minute).Format(time.RFC3339), CatchUpAnnotation: "never"})
			setTemplate(t, obj)
			objects := []client.Object{obj.DeepCopy()}
			if missing {
				objects = nil
			}
			r := &LifecycleReconciler{Client: fake.NewClientBuilder().WithObjects(objects...).Build(), Recorder: record.NewFakeRecorder(8), now: func() time.Time { return now }}
			if _, err := r.reconcileLogic(t.Context(), obj.DeepCopy(), logr.Discard()); err != nil {
				t.Fatal(err)
			}
			if missing {
				if _, err := r.Reconcile(t.Context(), timingRequest(obj)); err != nil {
					t.Fatal(err)
				}
			} else {
				noAction := obj.DeepCopy()
				noAction.SetAnnotations(nil)
				if _, err := r.reconcileLogic(t.Context(), noAction, logr.Discard()); err != nil {
					t.Fatal(err)
				}
			}
			policy, _ := ParseCatchUpPolicy("never")
			due, _ := time.Parse(time.RFC3339, obj.GetAnnotations()[RestartAtAnnotation])
			if r.observeOccurrence(obj, restartAction, policy, due, due.Add(time.Minute), false) {
				t.Fatal("removed resource/action retained eligibility")
			}
		})
	}
}
