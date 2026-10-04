package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// handleRestart implements the full restart logic with precedence.
func (r *LifecycleReconciler) handleRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (result ctrl.Result, err error) {
	defer func() {
		if err == nil && result.IsZero() {
			r.forgetTiming(obj)
		}
	}()
	policy, valid := r.catchUpPolicy(obj, restartAction, isDryRun)
	if !valid {
		return ctrl.Result{}, nil
	}
	if !supportsRestart(obj) {
		logger.Info("Skipping restart for an unsupported resource kind", "gvk", obj.GroupVersionKind())
		r.Recorder.Event(obj, "Warning", "UnsupportedRestartKind", "Restart requires an apps Deployment, StatefulSet, or DaemonSet; no action taken")
		return ctrl.Result{}, nil
	}
	if !hasRestartTemplate(obj) {
		logger.Info("Skipping restart because the Pod template is missing", "resource", client.ObjectKeyFromObject(obj))
		r.Recorder.Event(obj, "Warning", "NotPodSpawner", "Restart annotations are present but resource does not have a spec.template field.")
		return ctrl.Result{}, nil
	}

	annotations := obj.GetAnnotations()
	now := r.currentTime()

	if restartAfterStr := annotations[RestartAfterAnnotation]; restartAfterStr != "" {
		if annotations[ReferencePointAnnotation] == ReferencePointCreationTimestamp && annotations[RestartAtAnnotation] != "" {
			logger.Info("Ignoring restart-after because restart-at is already set with creationTimestamp reference point")
			if isDryRun {
				logger.Info("[DRY-RUN] Would remove redundant restart-after annotation")
				return ctrl.Result{}, nil
			}
			delete(annotations, RestartAfterAnnotation)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to remove redundant restart-after annotation")
				return ctrl.Result{}, err
			}
			r.armAbsolute(obj, restartAction, policy, RestartAtAnnotation, now)
			return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
		}

		duration, err := parseExtendedDuration(restartAfterStr)
		if err != nil {
			logger.Error(err, "invalid duration format for restart-after annotation", "value", restartAfterStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-after annotation: %v", err)
			return ctrl.Result{}, nil
		}
		referenceTime := r.getReferenceTime(obj, logger)
		restartTime := referenceTime.Add(duration)

		if isDryRun {
			logger.Info("[DRY-RUN] Would convert restart-after to restart-at without modifying the resource", "restartAfter", restartAfterStr, "calculatedRestartAt", restartTime.UTC().Format(time.RFC3339))
			r.Recorder.Eventf(obj, "Normal", "DryRunRestart", "Dry-run: Resource would be scheduled for restart at %s.", restartTime.UTC().Format(time.RFC3339))
			return ctrl.Result{}, nil
		}

		logger.Info("Converting restart-after to restart-at", "restartAfter", restartAfterStr, "calculatedRestartAt", restartTime.UTC().Format(time.RFC3339))
		newAnnotations := obj.GetAnnotations()
		if newAnnotations == nil {
			newAnnotations = make(map[string]string)
		}
		newAnnotations[RestartAtAnnotation] = restartTime.UTC().Format(time.RFC3339)
		delete(newAnnotations, RestartAfterAnnotation)
		obj.SetAnnotations(newAnnotations)
		markManagedBy(obj)
		if err := r.Update(ctx, obj); err != nil {
			logger.Error(err, "failed to update object with restart-at annotation")
			return ctrl.Result{}, err
		}
		r.armAbsolute(obj, restartAction, policy, RestartAtAnnotation, now)
		return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
	}

	if restartAtStr := annotations[RestartAtAnnotation]; restartAtStr != "" {
		restartTime, err := time.Parse(time.RFC3339, restartAtStr)
		if err != nil {
			logger.Error(err, "invalid format for restart-at annotation", "value", restartAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-at annotation: %v", err)
			return ctrl.Result{}, nil
		}
		allowed := r.observeOccurrence(obj, restartAction, policy, restartTime, now, isDryRun)
		if !now.Before(restartTime) {
			if !allowed {
				return ctrl.Result{}, r.skipAction(ctx, obj, restartAction, restartTime, isDryRun, func(a map[string]string) {
					delete(a, RestartAtAnnotation)
				})
			}
			logger.Info("Triggering one-time restart based on restart-at annotation", "restartTime", restartTime)
			if err := r.triggerRestart(ctx, obj, isDryRun, func(annotations map[string]string) {
				delete(annotations, RestartAtAnnotation)
			}, logger); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else {
			result := r.requeueForNextOccurrence(restartTime)
			logger.Info("One-time restart scheduled for a future time", "requeueAfter", result.RequeueAfter)
			return result, nil
		}
	}

	if cronStr := annotations[RestartCronAnnotation]; cronStr != "" {
		cronTimezone := annotations[CronTimezoneAnnotation]
		if cronTimezone == "" {
			cronTimezone = "UTC"
		}
		if _, err := time.LoadLocation(cronTimezone); err != nil {
			logger.Error(err, "invalid cron timezone", "timezone", cronTimezone)
			r.Recorder.Eventf(obj, "Warning", "InvalidTimezone", "Invalid timezone '%s' for restart-cron, taking no action.", cronTimezone)
			return ctrl.Result{}, nil
		}

		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		schedule, err := parser.Parse(fmt.Sprintf("CRON_TZ=%s %s", cronTimezone, cronStr))
		if err != nil {
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			return ctrl.Result{}, nil
		}
		if schedule.Next(now).IsZero() {
			err := fmt.Errorf("cron expression has no future occurrence")
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, "cron", cronRecurringSchedule{Schedule: schedule}, logger)
	}

	if everyStr := annotations[RestartEveryAnnotation]; everyStr != "" {
		duration, err := parseRestartEvery(everyStr)
		if err != nil {
			logger.Error(err, "invalid duration for restart-every", "duration", everyStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid duration for restart-every: %v", err)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, "interval", intervalRecurringSchedule{duration: duration}, logger)
	}

	return ctrl.Result{}, nil
}

// reconcileRecurringRestart handles the stateful logic for both cron and interval restarts.
func (r *LifecycleReconciler) reconcileRecurringRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, scheduleType string, schedule recurringSchedule, logger logr.Logger) (ctrl.Result, error) {
	policy, valid := r.catchUpPolicy(obj, restartAction, isDryRun)
	if !valid {
		return ctrl.Result{}, nil
	}
	annotations := obj.GetAnnotations()
	now := r.currentTime()
	lastRestartStr := annotations[LastRestartTimestamp]

	if lastRestartStr == "" {
		logger.Info("Initializing schedule by setting last-restart-timestamp", "type", scheduleType)
		if isDryRun {
			logger.Info("[DRY-RUN] Would initialize recurring restart state without modifying the resource", "type", scheduleType, "annotation", LastRestartTimestamp, "value", now.UTC().Format(time.RFC3339))
			return ctrl.Result{}, nil
		}
		annotations[LastRestartTimestamp] = now.UTC().Format(time.RFC3339Nano)
		obj.SetAnnotations(annotations)
		markManagedBy(obj)
		if err := r.Update(ctx, obj); err != nil {
			logger.Error(err, "failed to initialize last-restart-timestamp")
			return ctrl.Result{}, err
		}
		r.observeOccurrence(obj, restartAction, policy, schedule.Next(now), now, false)
		return r.requeueForNextOccurrence(schedule.Next(now)), nil
	}

	lastRestartTime, err := time.Parse(time.RFC3339, lastRestartStr)
	if err != nil {
		logger.Error(err, "failed to parse last-restart-timestamp", "value", lastRestartStr)
		r.forgetTiming(obj)
		r.Recorder.Eventf(obj, "Warning", "InvalidState", "Could not parse last-restart-timestamp: %v", err)
		return ctrl.Result{}, nil
	}

	nextScheduledRestart, coalescedAnchor, restartDue := planRecurringRestart(schedule, lastRestartTime, now)
	allowed := r.observeOccurrence(obj, restartAction, policy, nextScheduledRestart, now, isDryRun)
	if restartDue && policy.mode == catchUpDeadline {
		var recent time.Time
		recent, allowed = occurrenceWithinWindow(schedule, lastRestartTime, now, policy.window)
		if allowed {
			nextScheduledRestart = recent
		}
	}

	if restartDue {
		acknowledge := func(a map[string]string) {
			a[LastRestartTimestamp] = coalescedAnchor.UTC().Format(time.RFC3339Nano)
		}
		if !allowed {
			if err := r.skipAction(ctx, obj, restartAction, nextScheduledRestart, isDryRun, acknowledge); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			logger.Info("Triggering recurring restart", "type", scheduleType, "scheduledAt", nextScheduledRestart)
			if err := r.triggerRestart(ctx, obj, isDryRun, acknowledge, logger); err != nil {
				return ctrl.Result{}, err
			}
		}
		if isDryRun {
			return ctrl.Result{}, nil
		}
		r.forgetTiming(obj)
		r.observeOccurrence(obj, restartAction, policy, schedule.Next(coalescedAnchor), now, false)
		return r.requeueForNextOccurrence(schedule.Next(coalescedAnchor)), nil
	} else {
		result := r.requeueForNextOccurrence(nextScheduledRestart)
		logger.Info("Next recurring restart is scheduled", "type", scheduleType, "at", nextScheduledRestart, "requeueAfter", result.RequeueAfter)
		return result, nil
	}
}
