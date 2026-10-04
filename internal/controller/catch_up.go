package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type catchUpMode uint8

const (
	catchUpAlways catchUpMode = iota
	catchUpNever
	catchUpDeadline
	deleteAction  = "delete"
	restartAction = "restart"
)

// CatchUpPolicy controls eligibility after an action's scheduled time.
// The zero value permits catch-up without a deadline.
type CatchUpPolicy struct {
	mode   catchUpMode
	window time.Duration
}

// ParseCatchUpPolicy accepts always, never, or one positive integer with s, m, h, or d.
func ParseCatchUpPolicy(value string) (CatchUpPolicy, error) {
	switch value {
	case "always":
		return CatchUpPolicy{}, nil
	case "never":
		return CatchUpPolicy{mode: catchUpNever}, nil
	default:
		window, err := parseExtendedDuration(value)
		if err != nil {
			return CatchUpPolicy{}, fmt.Errorf("invalid catch-up policy %q: expected always, never, or a positive duration: %w", value, err)
		}
		return CatchUpPolicy{mode: catchUpDeadline, window: window}, nil
	}
}

// ActionTimingConfig supplies the default catch-up policy for each action.
type ActionTimingConfig struct {
	Delete  CatchUpPolicy
	Restart CatchUpPolicy
}

type occurrenceIdentity struct {
	uid    types.UID
	action string
	policy CatchUpPolicy
	config [4]string
}

type armedOccurrence struct {
	identity occurrenceIdentity
	due      time.Time
}

type actionTimers struct {
	mu    sync.Mutex
	armed map[resourceRequest]armedOccurrence
}

func timingRequest(obj client.Object) resourceRequest {
	return resourceRequest{NamespacedName: client.ObjectKeyFromObject(obj), GVK: obj.GetObjectKind().GroupVersionKind()}
}

func timingIdentity(obj client.Object, action string, policy CatchUpPolicy) occurrenceIdentity {
	a := obj.GetAnnotations()
	identity := occurrenceIdentity{uid: obj.GetUID(), action: action, policy: policy}
	switch {
	case action == deleteAction:
		identity.config = [4]string{DeleteAtAnnotation, a[DeleteAtAnnotation]}
	case a[RestartAtAnnotation] != "":
		identity.config = [4]string{RestartAtAnnotation, a[RestartAtAnnotation]}
	case a[RestartCronAnnotation] != "":
		identity.config = [4]string{RestartCronAnnotation, a[RestartCronAnnotation], a[CronTimezoneAnnotation], a[LastRestartTimestamp]}
	default:
		identity.config = [4]string{RestartEveryAnnotation, a[RestartEveryAnnotation], a[LastRestartTimestamp]}
	}
	return identity
}

func (r *LifecycleReconciler) forgetTiming(obj client.Object) {
	r.forgetRequestTiming(timingRequest(obj))
}

func (r *LifecycleReconciler) forgetRequestTiming(req resourceRequest) {
	r.timers.mu.Lock()
	defer r.timers.mu.Unlock()
	delete(r.timers.armed, req)
}

func (r *LifecycleReconciler) catchUpPolicy(obj client.Object, action string, dryRun bool) (CatchUpPolicy, bool) {
	policy := r.ActionTiming.Restart
	if action == deleteAction {
		policy = r.ActionTiming.Delete
	}
	if value, found := obj.GetAnnotations()[CatchUpAnnotation]; found {
		var err error
		policy, err = ParseCatchUpPolicy(value)
		if err != nil {
			r.forgetTiming(obj)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "%v for %s; taking no action", err, CatchUpAnnotation)
			return CatchUpPolicy{}, false
		}
	}
	if policy.mode != catchUpNever || dryRun {
		r.forgetTiming(obj)
	}
	return policy, true
}

// observeOccurrence arms future work only in this process. A matching arm
// survives API errors and queue delays until the occurrence is acknowledged.
func (r *LifecycleReconciler) observeOccurrence(obj client.Object, action string, policy CatchUpPolicy, due, now time.Time, dryRun bool) bool {
	switch policy.mode {
	case catchUpAlways:
		return true
	case catchUpDeadline:
		return !now.After(due.Add(policy.window))
	}
	if dryRun {
		return false
	}
	key := timingRequest(obj)
	identity := timingIdentity(obj, action, policy)
	r.timers.mu.Lock()
	defer r.timers.mu.Unlock()
	armed, found := r.timers.armed[key]
	if found && (armed.identity != identity || !armed.due.Equal(due)) {
		delete(r.timers.armed, key)
		found = false
	}
	if due.After(now) {
		if r.timers.armed == nil {
			r.timers.armed = make(map[resourceRequest]armedOccurrence)
		}
		r.timers.armed[key] = armedOccurrence{identity: identity, due: due}
		return true
	}
	return found
}

func (r *LifecycleReconciler) armAbsolute(obj client.Object, action string, policy CatchUpPolicy, annotation string, observedAt time.Time) {
	due, err := time.Parse(time.RFC3339, obj.GetAnnotations()[annotation])
	if err == nil {
		r.observeOccurrence(obj, action, policy, due, observedAt, false)
	}
}

// occurrenceWithinWindow checks recent work without enumerating missed history.
func occurrenceWithinWindow(schedule recurringSchedule, lastRestart, now time.Time, window time.Duration) (time.Time, bool) {
	cutoff := now.Add(-window)
	if interval, ok := schedule.(intervalRecurringSchedule); ok {
		latest := interval.coalescedAnchor(lastRestart, now)
		return latest, latest.After(lastRestart) && !latest.Before(cutoff)
	}
	lower := cutoff.Add(-time.Nanosecond) // Include the exact deadline boundary.
	if lastRestart.After(lower) {
		lower = lastRestart
	}
	occurrence := schedule.Next(lower)
	return occurrence, !occurrence.IsZero() && !occurrence.After(now)
}

// skipAction consumes one occurrence with an optimistic patch. It never changes
// the Pod template and cannot acknowledge a concurrent schedule edit.
func (r *LifecycleReconciler) skipAction(ctx context.Context, obj *unstructured.Unstructured, action string, due time.Time, dryRun bool, acknowledge func(map[string]string)) error {
	if dryRun {
		r.Recorder.Eventf(obj, "Normal", "DryRunActionSkipped", "Dry-run: Would skip %s scheduled at %s under the catch-up policy.", action, due.UTC().Format(time.RFC3339Nano))
		return nil
	}
	base := obj.DeepCopy()
	a := obj.GetAnnotations()
	acknowledge(a)
	obj.SetAnnotations(a)
	markManagedBy(obj)
	if err := r.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		r.Recorder.Eventf(obj, "Warning", "ActionSkipFailed", "Could not acknowledge skipped %s: %v", action, err)
		return err
	}
	r.forgetTiming(obj)
	r.Recorder.Eventf(obj, "Normal", "ActionSkipped", "Skipped %s scheduled at %s under the catch-up policy.", action, due.UTC().Format(time.RFC3339Nano))
	return nil
}
