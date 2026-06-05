package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *LifecycleReconciler) handleDeletion(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()

	if deleteAfterStr := annotations[DeleteAfterAnnotation]; deleteAfterStr != "" {
		if annotations[ReferencePointAnnotation] == ReferencePointCreationTimestamp && annotations[DeleteAtAnnotation] != "" {
			logger.Info("Ignoring delete-after because delete-at is already set with creationTimestamp reference point")
			delete(annotations, DeleteAfterAnnotation)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to remove redundant delete-after annotation")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		duration, err := parseExtendedDuration(deleteAfterStr)
		if err != nil {
			logger.Error(err, "invalid duration format for delete-after annotation", "value", deleteAfterStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for delete-after annotation: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}
		referenceTime := r.getReferenceTime(obj, logger)
		deletionTime := referenceTime.Add(duration)

		logger.Info("Converting delete-after to delete-at", "deleteAfter", deleteAfterStr, "calculatedDeleteAt", deletionTime.UTC().Format(time.RFC3339))
		newAnnotations := obj.GetAnnotations()
		if newAnnotations == nil {
			newAnnotations = make(map[string]string)
		}
		newAnnotations[DeleteAtAnnotation] = deletionTime.UTC().Format(time.RFC3339)
		delete(newAnnotations, DeleteAfterAnnotation)
		obj.SetAnnotations(newAnnotations)
		markManagedBy(obj)

		if err := r.Update(ctx, obj); err != nil {
			logger.Error(err, "failed to update object with delete-at annotation")
			return ctrl.Result{}, err
		}
		r.lifecycleMetrics().observeNextAction(obj, metricActionDelete, deletionTime)
		return ctrl.Result{Requeue: true}, nil
	}

	if deleteAtStr := annotations[DeleteAtAnnotation]; deleteAtStr != "" {
		deleteAtTime, err := time.Parse(time.RFC3339, deleteAtStr)
		if err != nil {
			logger.Error(err, "invalid format for delete-at annotation", "value", deleteAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for delete-at annotation: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}

		if time.Now().After(deleteAtTime) {
			logger.Info("Deleting resource based on delete-at annotation", "targetTime", deleteAtTime.String())
			if !isDryRun {
				if err := r.Delete(ctx, obj); err != nil {
					if !apierrors.IsNotFound(err) {
						logger.Error(err, "failed to delete object")
						r.Recorder.Eventf(obj, "Warning", "DeletionFailed", "Failed to delete resource: %v", err)
						r.lifecycleMetrics().recordAction(obj, metricActionDelete, metricResultError)
						return ctrl.Result{}, err
					}
				}
				r.Recorder.Eventf(obj, "Normal", "Deleted", "Resource deleted based on delete-at annotation: %s", deleteAtStr)
				r.lifecycleMetrics().recordAction(obj, metricActionDelete, metricResultSuccess)
			} else {
				logger.Info("[DRY-RUN] Would delete resource now")
				r.Recorder.Event(obj, "Normal", "DryRunDelete", "Dry-run: Resource would be deleted now.")
				r.lifecycleMetrics().recordAction(obj, metricActionDelete, metricResultDryRun)
			}
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		} else {
			requeueAfter := time.Until(deleteAtTime)
			logger.Info("Resource deletion scheduled for a future time", "requeueAfter", requeueAfter)
			r.lifecycleMetrics().observeNextAction(obj, metricActionDelete, deleteAtTime)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	r.lifecycleMetrics().clearNextAction(obj)
	return ctrl.Result{}, nil
}

// handleRestart implements the full restart logic with precedence.
func (r *LifecycleReconciler) handleRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (ctrl.Result, error) {
	_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template")
	if err != nil || !found {
		logger.Info("Resource is not a pod-spawner, skipping restart action.", "resource", client.ObjectKeyFromObject(obj))
		r.Recorder.Event(obj, "Warning", "NotPodSpawner", "Restart annotations are present but resource does not have a spec.template field.")
		r.lifecycleMetrics().recordMisconfiguration(obj, "not_pod_spawner")
		r.lifecycleMetrics().clearNextAction(obj)
		return ctrl.Result{}, nil
	}

	annotations := obj.GetAnnotations()
	now := time.Now()

	if restartAfterStr := annotations[RestartAfterAnnotation]; restartAfterStr != "" {
		if annotations[ReferencePointAnnotation] == ReferencePointCreationTimestamp && annotations[RestartAtAnnotation] != "" {
			logger.Info("Ignoring restart-after because restart-at is already set with creationTimestamp reference point")
			delete(annotations, RestartAfterAnnotation)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to remove redundant restart-after annotation")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		duration, err := parseExtendedDuration(restartAfterStr)
		if err != nil {
			logger.Error(err, "invalid duration format for restart-after annotation", "value", restartAfterStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-after annotation: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}
		referenceTime := r.getReferenceTime(obj, logger)
		restartTime := referenceTime.Add(duration)

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
		r.lifecycleMetrics().observeNextAction(obj, metricActionRestart, restartTime)
		return ctrl.Result{Requeue: true}, nil
	}

	if restartAtStr := annotations[RestartAtAnnotation]; restartAtStr != "" {
		restartTime, err := time.Parse(time.RFC3339, restartAtStr)
		if err != nil {
			logger.Error(err, "invalid format for restart-at annotation", "value", restartAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-at annotation: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}
		if now.After(restartTime) {
			logger.Info("Triggering one-time restart based on restart-at annotation", "restartTime", restartTime)
			if err := r.triggerRestart(ctx, obj, isDryRun, logger); err != nil {
				return ctrl.Result{}, err
			}
			if !isDryRun {
				delete(annotations, RestartAtAnnotation)
				obj.SetAnnotations(annotations)
				markManagedBy(obj)
				if err := r.Update(ctx, obj); err != nil {
					logger.Error(err, "failed to remove restart-at annotation")
					return ctrl.Result{}, err
				}
			}
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		} else {
			requeueAfter := time.Until(restartTime)
			logger.Info("One-time restart scheduled for a future time", "requeueAfter", requeueAfter)
			r.lifecycleMetrics().observeNextAction(obj, metricActionRestart, restartTime)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
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
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_timezone")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}

		schedule, err := cron.ParseStandard(fmt.Sprintf("CRON_TZ=%s %s", cronTimezone, cronStr))
		if err != nil {
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, "cron", schedule, logger)
	}

	if everyStr := annotations[RestartEveryAnnotation]; everyStr != "" {
		duration, err := parseExtendedDuration(everyStr)
		if err != nil {
			logger.Error(err, "invalid duration for restart-every", "duration", everyStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid duration for restart-every: %v", err)
			r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_annotation")
			r.lifecycleMetrics().clearNextAction(obj)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, "interval", duration, logger)
	}

	return ctrl.Result{}, nil
}

// reconcileRecurringRestart handles the stateful logic for both cron and interval restarts.
func (r *LifecycleReconciler) reconcileRecurringRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, scheduleType string, schedule interface{}, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()
	now := time.Now()
	lastRestartStr := annotations[LastRestartTimestamp]

	if lastRestartStr == "" {
		logger.Info("Initializing schedule by setting last-restart-timestamp", "type", scheduleType)
		if !isDryRun {
			annotations[LastRestartTimestamp] = now.UTC().Format(time.RFC3339)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to initialize last-restart-timestamp")
				return ctrl.Result{}, err
			}
		}
		r.lifecycleMetrics().observeNextAction(obj, metricActionRestart, nextRestartTime(scheduleType, schedule, now))
		return ctrl.Result{Requeue: true}, nil
	}

	lastRestartTime, err := time.Parse(time.RFC3339, lastRestartStr)
	if err != nil {
		logger.Error(err, "failed to parse last-restart-timestamp", "value", lastRestartStr)
		r.Recorder.Eventf(obj, "Warning", "InvalidState", "Could not parse last-restart-timestamp: %v", err)
		r.lifecycleMetrics().recordMisconfiguration(obj, "invalid_state")
		r.lifecycleMetrics().clearNextAction(obj)
		return ctrl.Result{}, nil
	}

	nextScheduledRestart := nextRestartTime(scheduleType, schedule, lastRestartTime)

	if now.After(nextScheduledRestart) {
		logger.Info("Triggering recurring restart", "type", scheduleType, "scheduledAt", nextScheduledRestart)
		if err := r.triggerRestart(ctx, obj, isDryRun, logger); err != nil {
			return ctrl.Result{}, err
		}
		if !isDryRun {
			annotations[LastRestartTimestamp] = nextScheduledRestart.Format(time.RFC3339)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to update last-restart-timestamp after restart")
				return ctrl.Result{}, err
			}
		}
		r.lifecycleMetrics().observeNextAction(obj, metricActionRestart, nextRestartTime(scheduleType, schedule, nextScheduledRestart))
		return ctrl.Result{Requeue: true}, nil
	} else {
		requeueAfter := time.Until(nextScheduledRestart)
		logger.Info("Next recurring restart is scheduled", "type", scheduleType, "at", nextScheduledRestart, "requeueAfter", requeueAfter)
		r.lifecycleMetrics().observeNextAction(obj, metricActionRestart, nextScheduledRestart)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
}

// triggerRestart injects the annotation into the pod template to cause a rollout.
func (r *LifecycleReconciler) triggerRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) error {
	restartedAtTime := time.Now().UTC().Format(time.RFC3339)
	logger.Info("Attempting to trigger restart", "restartedAt", restartedAtTime)

	if isDryRun {
		logger.Info("[DRY-RUN] Would trigger restart by setting template annotation", "annotation", RestartedAtTemplate, "value", restartedAtTime)
		r.Recorder.Event(obj, "Normal", "DryRunRestart", "Dry-run: Resource would be restarted now.")
		r.lifecycleMetrics().recordAction(obj, metricActionRestart, metricResultDryRun)
		return nil
	}

	templateAnnotations, found, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		logger.Error(err, "failed to get spec.template.metadata.annotations")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not read pod template annotations: %v", err)
		return err
	}
	if !found || templateAnnotations == nil {
		templateAnnotations = make(map[string]string)
	}

	templateAnnotations[RestartedAtTemplate] = restartedAtTime
	templateAnnotations[ManagedByAnnotation] = ManagedByValue
	err = unstructured.SetNestedStringMap(obj.Object, templateAnnotations, "spec", "template", "metadata", "annotations")
	if err != nil {
		logger.Error(err, "failed to set restartedAt annotation on pod template")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not set pod template annotations: %v", err)
		return err
	}
	markManagedBy(obj)

	if err := r.Update(ctx, obj); err != nil {
		logger.Error(err, "failed to update object to trigger restart")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not update object to trigger restart: %v", err)
		r.lifecycleMetrics().recordAction(obj, metricActionRestart, metricResultError)
		return err
	}

	logger.Info("Successfully updated object to trigger restart")
	r.Recorder.Event(obj, "Normal", "RestartTriggered", "Triggered a rolling restart of the resource.")
	r.lifecycleMetrics().recordAction(obj, metricActionRestart, metricResultSuccess)
	return nil
}
