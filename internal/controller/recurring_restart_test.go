package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type patchCountingClient struct {
	client.Client
	patches int
}

func (c *patchCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestRestartEveryValidationStopsPermanentRetries(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantEvent string
	}{
		{name: "zero", value: "0s", wantEvent: "at least 1m0s"},
		{name: "negative", value: "-1m", wantEvent: "must be positive"},
		{name: "below minimum", value: "59s", wantEvent: "at least 1m0s"},
		{name: "overflowing days", value: "768614336404564651d", wantEvent: "too large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			key := types.NamespacedName{Name: "invalid-restart-every", Namespace: "default"}
			obj := testDeployment(key.Name, map[string]string{RestartEveryAnnotation: tt.value})
			setTemplate(t, obj)
			recorder := record.NewFakeRecorder(1)
			r := &LifecycleReconciler{
				Client:   fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
				Recorder: recorder,
			}

			result, err := r.handleRestart(ctx, obj, false, logr.Discard())
			if err != nil {
				t.Fatalf("handle restart: %v", err)
			}
			if !result.IsZero() {
				t.Fatalf("result = %+v, want no requeue for permanent invalid configuration", result)
			}

			fetched := getObject(t, r, key)
			if _, found := fetched.GetAnnotations()[LastRestartTimestamp]; found {
				t.Fatalf("last restart timestamp initialized for invalid interval")
			}
			templateAnnotations, _, err := unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
			if err != nil {
				t.Fatalf("get template annotations: %v", err)
			}
			if _, found := templateAnnotations[RestartedAtTemplate]; found {
				t.Fatalf("restart triggered for invalid interval")
			}

			select {
			case event := <-recorder.Events:
				if !strings.Contains(event, "InvalidAnnotation") || !strings.Contains(event, tt.wantEvent) {
					t.Fatalf("event = %q, want InvalidAnnotation containing %q", event, tt.wantEvent)
				}
			case <-time.After(time.Second):
				t.Fatal("expected invalid annotation event")
			}
		})
	}
}

func TestRestartEveryMinimumIsAccepted(t *testing.T) {
	duration, err := parseRestartEvery("1m")
	if err != nil {
		t.Fatalf("parse minimum restart interval: %v", err)
	}
	if duration != time.Minute {
		t.Fatalf("duration = %s, want %s", duration, time.Minute)
	}
}

func TestRestartCronRejectsSubMinuteDescriptorWithoutRetry(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "invalid-restart-cron", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{RestartCronAnnotation: "@every 1s"})
	setTemplate(t, obj)
	recorder := record.NewFakeRecorder(1)
	r := &LifecycleReconciler{
		Client:   fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
		Recorder: recorder,
	}

	result, err := r.handleRestart(ctx, obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle restart: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("result = %+v, want no requeue for permanent invalid configuration", result)
	}

	fetched := getObject(t, r, key)
	if _, found := fetched.GetAnnotations()[LastRestartTimestamp]; found {
		t.Fatalf("last restart timestamp initialized for invalid cron descriptor")
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "InvalidAnnotation") {
			t.Fatalf("event = %q, want InvalidAnnotation", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected invalid annotation event")
	}
}

func TestRestartCronRejectsScheduleWithoutFutureOccurrence(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "impossible-restart-cron", Namespace: "default"}
	obj := testDeployment(key.Name, map[string]string{RestartCronAnnotation: "0 0 31 2 *"})
	setTemplate(t, obj)
	recorder := record.NewFakeRecorder(1)
	r := &LifecycleReconciler{
		Client:   fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build(),
		Recorder: recorder,
	}

	result, err := r.handleRestart(ctx, obj, false, logr.Discard())
	if err != nil {
		t.Fatalf("handle restart: %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("result = %+v, want no requeue for schedule without a future occurrence", result)
	}

	fetched := getObject(t, r, key)
	if _, found := fetched.GetAnnotations()[LastRestartTimestamp]; found {
		t.Fatalf("last restart timestamp initialized for schedule without a future occurrence")
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "InvalidAnnotation") || !strings.Contains(event, "no future occurrence") {
			t.Fatalf("event = %q, want terminal InvalidAnnotation", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected invalid annotation event")
	}
}

func TestElapsedNextOccurrenceRequeuesImmediately(t *testing.T) {
	result := requeueForNextOccurrence(time.Now().Add(-time.Second))
	if result.RequeueAfter != time.Nanosecond {
		t.Fatalf("result = %+v, want immediate requeue for elapsed occurrence", result)
	}
}

func TestRecurringRestartPlanning(t *testing.T) {
	lastRestart := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	interval := intervalRecurringSchedule{duration: time.Minute}

	t.Run("normal interval waits for its next occurrence", func(t *testing.T) {
		scheduledAt, anchor, due := planRecurringRestart(interval, lastRestart, lastRestart.Add(59*time.Second))
		if due {
			t.Fatal("restart reported due before interval elapsed")
		}
		if want := lastRestart.Add(time.Minute); !scheduledAt.Equal(want) {
			t.Fatalf("scheduled at %s, want %s", scheduledAt, want)
		}
		if !anchor.Equal(lastRestart) {
			t.Fatalf("anchor = %s, want unchanged %s", anchor, lastRestart)
		}
	})

	t.Run("exact interval boundary is due", func(t *testing.T) {
		now := lastRestart.Add(time.Minute)
		_, anchor, due := planRecurringRestart(interval, lastRestart, now)
		if !due {
			t.Fatal("restart not due at exact boundary")
		}
		if !anchor.Equal(now) {
			t.Fatalf("anchor = %s, want boundary %s", anchor, now)
		}
	})

	t.Run("many missed intervals advance to latest elapsed anchor", func(t *testing.T) {
		now := lastRestart.Add(24*time.Hour + 30*time.Second)
		scheduledAt, anchor, due := planRecurringRestart(interval, lastRestart, now)
		if !due {
			t.Fatal("restart not due after long downtime")
		}
		if want := lastRestart.Add(time.Minute); !scheduledAt.Equal(want) {
			t.Fatalf("first missed occurrence = %s, want %s", scheduledAt, want)
		}
		if want := lastRestart.Add(24 * time.Hour); !anchor.Equal(want) {
			t.Fatalf("coalesced anchor = %s, want %s", anchor, want)
		}
		if _, _, dueAgain := planRecurringRestart(interval, anchor, now); dueAgain {
			t.Fatal("coalesced interval remains due")
		}
	})

	t.Run("fractional interval preserves cadence", func(t *testing.T) {
		fractional := intervalRecurringSchedule{duration: 60*time.Second + 500*time.Millisecond}
		now := lastRestart.Add(2*time.Minute + 800*time.Millisecond)
		_, anchor, due := planRecurringRestart(fractional, lastRestart, now)
		if !due {
			t.Fatal("restart not due after fractional interval elapsed")
		}
		if want := lastRestart.Add(60*time.Second + 500*time.Millisecond); !anchor.Equal(want) {
			t.Fatalf("coalesced anchor = %s, want %s", anchor, want)
		}
		if _, _, dueAgain := planRecurringRestart(fractional, anchor, now); dueAgain {
			t.Fatal("fractional coalesced interval remains due")
		}
	})

	t.Run("ancient anchor coalesces without duration saturation", func(t *testing.T) {
		ancient := time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
		now := time.Date(2026, time.July, 14, 12, 0, 30, 0, time.UTC)
		_, anchor, due := planRecurringRestart(interval, ancient, now)
		if !due {
			t.Fatal("restart not due for ancient anchor")
		}
		if lag := now.Sub(anchor); lag < 0 || lag >= time.Minute {
			t.Fatalf("coalesced anchor lags current time by %s, want less than one interval", lag)
		}
		if _, _, dueAgain := planRecurringRestart(interval, anchor, now); dueAgain {
			t.Fatal("ancient coalesced interval remains due")
		}
	})

	cronSchedule, err := cron.ParseStandard("0 * * * *")
	if err != nil {
		t.Fatalf("parse cron schedule: %v", err)
	}
	recurringCron := cronRecurringSchedule{Schedule: cronSchedule}

	t.Run("normal cron waits for its next occurrence", func(t *testing.T) {
		now := lastRestart.Add(30 * time.Minute)
		scheduledAt, anchor, due := planRecurringRestart(recurringCron, lastRestart, now)
		if due {
			t.Fatal("cron restart reported due before scheduled time")
		}
		if want := lastRestart.Add(time.Hour); !scheduledAt.Equal(want) {
			t.Fatalf("scheduled at %s, want %s", scheduledAt, want)
		}
		if !anchor.Equal(lastRestart) {
			t.Fatalf("anchor = %s, want unchanged %s", anchor, lastRestart)
		}
	})

	t.Run("missed cron occurrences coalesce at the current anchor", func(t *testing.T) {
		now := lastRestart.Add(3*time.Hour + 30*time.Minute)
		_, anchor, due := planRecurringRestart(recurringCron, lastRestart, now)
		if !due {
			t.Fatal("cron restart not due after missed occurrences")
		}
		if !anchor.Equal(now) {
			t.Fatalf("coalesced anchor = %s, want current time %s", anchor, now)
		}
		if _, _, dueAgain := planRecurringRestart(recurringCron, anchor, now); dueAgain {
			t.Fatal("coalesced cron remains due")
		}
	})

	t.Run("exact cron boundary is due", func(t *testing.T) {
		now := lastRestart.Add(time.Hour)
		_, anchor, due := planRecurringRestart(recurringCron, lastRestart, now)
		if !due {
			t.Fatal("cron restart not due at exact boundary")
		}
		if !anchor.Equal(now) {
			t.Fatalf("coalesced anchor = %s, want boundary %s", anchor, now)
		}
	})
}

func TestRecurringRestartCoalescesMissedIntervalsIntoOneRollout(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "coalesced-restart-every", Namespace: "default"}
	base := time.Now().UTC().Truncate(time.Second)
	lastRestart := base.Add(-24*time.Hour - 30*time.Second)
	expectedAnchor := lastRestart.Add(24 * time.Hour)
	obj := testDeployment(key.Name, map[string]string{
		RestartEveryAnnotation: "1m",
		LastRestartTimestamp:   lastRestart.Format(time.RFC3339),
	})
	obj.SetResourceVersion("1")
	setTemplate(t, obj)
	countingClient := &patchCountingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
	r := &LifecycleReconciler{
		Client:   countingClient,
		Recorder: record.NewFakeRecorder(2),
	}
	schedule := intervalRecurringSchedule{duration: time.Minute}

	result, err := r.reconcileRecurringRestart(ctx, obj, false, "interval", schedule, logr.Discard())
	if err != nil {
		t.Fatalf("reconcile recurring restart: %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Minute {
		t.Fatalf("requeue after = %s, want next future interval", result.RequeueAfter)
	}
	if countingClient.patches != 1 {
		t.Fatalf("patches = %d, want one atomic rollout and anchor patch", countingClient.patches)
	}

	fetched := getObject(t, r, key)
	anchor, err := time.Parse(time.RFC3339, fetched.GetAnnotations()[LastRestartTimestamp])
	if err != nil {
		t.Fatalf("parse coalesced anchor: %v", err)
	}
	if !anchor.Equal(expectedAnchor) {
		t.Fatalf("coalesced anchor = %s, want %s", anchor, expectedAnchor)
	}
	templateAnnotations, _, err := unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations: %v", err)
	}
	firstRestartedAt := templateAnnotations[RestartedAtTemplate]
	if firstRestartedAt == "" {
		t.Fatal("restart was not triggered")
	}

	result, err = r.reconcileRecurringRestart(ctx, fetched, false, "interval", schedule, logr.Discard())
	if err != nil {
		t.Fatalf("reconcile after coalescing: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeue after coalescing = %s, want future schedule", result.RequeueAfter)
	}
	if countingClient.patches != 1 {
		t.Fatalf("patches after second reconcile = %d, want no catch-up patch", countingClient.patches)
	}

	fetched = getObject(t, r, key)
	templateAnnotations, _, err = unstructured.NestedStringMap(fetched.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("get template annotations after second reconcile: %v", err)
	}
	if got := templateAnnotations[RestartedAtTemplate]; got != firstRestartedAt {
		t.Fatalf("second rollout triggered: restartedAt changed from %q to %q", firstRestartedAt, got)
	}
}
