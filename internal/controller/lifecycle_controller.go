package controller

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Constants for annotations
const (
	CronTimezoneAnnotation          = "lifecycle.cezary.dev/cron-timezone"
	DryRunAnnotation                = "lifecycle.cezary.dev/dry-run"
	ReferencePointAnnotation        = "lifecycle.cezary.dev/reference-point"
	DeleteAtAnnotation              = "lifecycle.cezary.dev/delete-at"
	DeleteAfterAnnotation           = "lifecycle.cezary.dev/delete-after"
	RestartAtAnnotation             = "lifecycle.cezary.dev/restart-at"
	RestartAfterAnnotation          = "lifecycle.cezary.dev/restart-after"
	RestartCronAnnotation           = "lifecycle.cezary.dev/restart-cron"
	RestartEveryAnnotation          = "lifecycle.cezary.dev/restart-every"
	LastRestartTimestamp            = "lifecycle.cezary.dev/last-restart-timestamp"
	RestartedAtTemplate             = "lifecycle.cezary.dev/restartedAt"
	ManagedByAnnotation             = "lifecycle.cezary.dev/managed-by"
	ManagedByValue                  = "lifecycle-controller"
	ReferencePointCreationTimestamp = "creationTimestamp"
	minimumRestartEveryInterval     = time.Minute
)

type resourceRequest struct {
	types.NamespacedName
	GVK schema.GroupVersionKind
}

// recurringSchedule describes the two decisions needed by recurring restart
// reconciliation: the next occurrence and the anchor to persist after missed
// occurrences are coalesced.
type recurringSchedule interface {
	Next(time.Time) time.Time
	coalescedAnchor(lastRestart, now time.Time) time.Time
}

type intervalRecurringSchedule struct {
	duration time.Duration
}

func (s intervalRecurringSchedule) Next(anchor time.Time) time.Time {
	return anchor.Add(s.duration)
}

func (s intervalRecurringSchedule) coalescedAnchor(lastRestart, now time.Time) time.Time {
	return now.Add(-elapsedModulo(lastRestart, now, s.duration))
}

func elapsedModulo(start, end time.Time, interval time.Duration) time.Duration {
	// time.Time.Sub saturates at the largest duration, so retain only each
	// chunk's modulo while advancing across anchors farther than ~292 years.
	var remainder time.Duration
	for cursor := start; cursor.Before(end); {
		chunk := end.Sub(cursor)
		next := end
		if chunk == time.Duration(math.MaxInt64) {
			next = cursor.Add(chunk)
		}

		chunk %= interval
		if remainder >= interval-chunk {
			remainder -= interval - chunk
		} else {
			remainder += chunk
		}
		cursor = next
	}
	return remainder
}

type cronRecurringSchedule struct {
	cron.Schedule
}

func (cronRecurringSchedule) coalescedAnchor(_ time.Time, now time.Time) time.Time {
	return now
}

func planRecurringRestart(schedule recurringSchedule, lastRestart, now time.Time) (scheduledAt, coalescedAnchor time.Time, due bool) {
	scheduledAt = schedule.Next(lastRestart)
	if now.Before(scheduledAt) {
		return scheduledAt, lastRestart, false
	}
	return scheduledAt, schedule.coalescedAnchor(lastRestart, now), true
}

func requeueForNextOccurrence(next time.Time) ctrl.Result {
	if next.IsZero() {
		return ctrl.Result{}
	}
	requeueAfter := time.Until(next)
	if requeueAfter <= 0 {
		requeueAfter = time.Nanosecond
	}
	return ctrl.Result{RequeueAfter: requeueAfter}
}

// LifecycleReconciler reconciles objects with lifecycle annotations.
type LifecycleReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     eventRecorder
	Config       ScopeConfig
	GlobalDryRun bool
	discovery    preferredResourceDiscovery
	coverage     *coverageState
}

// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// parseExtendedDuration enhances time.ParseDuration to support 'd' for days.
func parseExtendedDuration(durationStr string) (time.Duration, error) {
	// Regex to find number and unit, specifically looking for 'd'
	re := regexp.MustCompile(`(\d+)\s*d`)
	matches := re.FindAllStringSubmatch(durationStr, -1)

	var dayDuration time.Duration
	// Replace day components with hour components
	processedStr := durationStr
	for _, match := range matches {
		if len(match) == 2 {
			days, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid number of days: %s", match[1])
			}
			if days > (math.MaxInt64-int64(dayDuration))/int64(24*time.Hour) {
				return 0, fmt.Errorf("number of days is too large: %s", match[1])
			}
			dayDuration += time.Duration(days) * 24 * time.Hour
			processedStr = strings.Replace(processedStr, match[0], "", 1)
		}
	}

	// Remove spaces to avoid parsing issues with the remaining string
	processedStr = strings.ReplaceAll(processedStr, " ", "")

	if processedStr == "" {
		if dayDuration > 0 {
			return dayDuration, nil
		}
		return 0, fmt.Errorf("duration string '%s' is empty or invalid", durationStr)
	}

	remainder, err := time.ParseDuration(processedStr)
	if err != nil {
		return 0, err
	}
	if remainder > 0 && dayDuration > time.Duration(math.MaxInt64)-remainder {
		return 0, fmt.Errorf("duration is too large: %s", durationStr)
	}
	return dayDuration + remainder, nil
}

func parseRestartEvery(durationStr string) (time.Duration, error) {
	if strings.HasPrefix(strings.TrimSpace(durationStr), "-") {
		return 0, fmt.Errorf("duration must be positive")
	}
	duration, err := parseExtendedDuration(durationStr)
	if err != nil {
		return 0, err
	}
	if duration < minimumRestartEveryInterval {
		return 0, fmt.Errorf("duration must be at least %s", minimumRestartEveryInterval)
	}
	return duration, nil
}

// getReferenceTime determines the starting point for a relative timer based on annotations.
func (r *LifecycleReconciler) getReferenceTime(obj client.Object, logger logr.Logger) time.Time {
	annotations := obj.GetAnnotations()
	referencePoint := annotations[ReferencePointAnnotation]

	switch referencePoint {
	case ReferencePointCreationTimestamp:
		refTime := obj.GetCreationTimestamp().UTC()
		logger.Info("Using creationTimestamp as reference point", "timestamp", refTime)
		return refTime
	case "applyTimestamp", "": // Default behavior
		if referencePoint == "" {
			logger.Info("No reference-point specified, defaulting to 'applyTimestamp' (reconciliation time)")
		} else {
			logger.Info("Using 'applyTimestamp' (reconciliation time) as reference point")
		}
		return time.Now().UTC()
	default:
		logger.Info("Invalid reference-point specified, falling back to 'applyTimestamp'", "value", referencePoint)
		r.Recorder.Eventf(obj, "Warning", "InvalidAnnotationValue", "Invalid value for %s: '%s', falling back to 'applyTimestamp'", ReferencePointAnnotation, referencePoint)
		return time.Now().UTC()
	}
}

func markManagedBy(obj client.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	obj.SetAnnotations(annotations)
}

func (r *LifecycleReconciler) dryRunEnabled(obj client.Object) (bool, error) {
	dryRunValue, found := obj.GetAnnotations()[DryRunAnnotation]
	if !found {
		return r.GlobalDryRun, nil
	}

	resourceDryRun, err := strconv.ParseBool(dryRunValue)
	if err != nil {
		return false, fmt.Errorf("invalid value %q for %s: expected a boolean", dryRunValue, DryRunAnnotation)
	}
	return r.GlobalDryRun || resourceDryRun, nil
}

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req resourceRequest) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(req.GVK)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("object not found, likely deleted", "gvk", req.GVK)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get object", "gvk", req.GVK)
		return ctrl.Result{}, err
	}

	return r.reconcileLogic(ctx, obj, logger)
}

func (r *LifecycleReconciler) reconcileLogic(ctx context.Context, obj client.Object, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()
	if len(annotations) == 0 {
		return ctrl.Result{}, nil
	}

	// The object from Reconcile is already unstructured, but we ensure it's the correct type.
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		// This is a fallback, should not happen with the new Reconcile logic
		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			logger.Error(err, "could not convert object to unstructured")
			return ctrl.Result{}, err
		}
		u = &unstructured.Unstructured{Object: unstructuredObj}
	}

	isDryRun, err := r.dryRunEnabled(obj)
	if err != nil {
		logger.Error(err, "invalid dry-run annotation; taking no action")
		r.Recorder.Eventf(obj, "Warning", "InvalidAnnotationValue", "%v; taking no action", err)
		return ctrl.Result{}, nil
	}
	hasRestartCron := annotations[RestartCronAnnotation] != ""
	if annotations[CronTimezoneAnnotation] != "" && !hasRestartCron {
		r.Recorder.Eventf(obj, "Warning", "IgnoredAnnotation", "Ignoring %s because %s is not set.", CronTimezoneAnnotation, RestartCronAnnotation)
	}

	hasDeleteAnno := annotations[DeleteAtAnnotation] != "" || annotations[DeleteAfterAnnotation] != ""
	hasRestartAnno := annotations[RestartAtAnnotation] != "" || annotations[RestartAfterAnnotation] != "" || annotations[RestartCronAnnotation] != "" || annotations[RestartEveryAnnotation] != ""

	if hasDeleteAnno && hasRestartAnno {
		logger.Info("Conflict: Resource has both delete and restart annotations. Taking no action.", "resource", client.ObjectKeyFromObject(obj))
		r.Recorder.Event(obj, "Warning", "ConflictingAnnotations", "Resource has both delete and restart annotations.")
		return ctrl.Result{}, nil
	}

	if hasDeleteAnno {
		return r.handleDeletion(ctx, u, isDryRun, logger)
	}
	if hasRestartAnno {
		return r.handleRestart(ctx, u, isDryRun, logger)
	}

	return ctrl.Result{}, nil
}

func (r *LifecycleReconciler) handleDeletion(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()

	if deleteAfterStr := annotations[DeleteAfterAnnotation]; deleteAfterStr != "" {
		if annotations[ReferencePointAnnotation] == ReferencePointCreationTimestamp && annotations[DeleteAtAnnotation] != "" {
			logger.Info("Ignoring delete-after because delete-at is already set with creationTimestamp reference point")
			if isDryRun {
				logger.Info("[DRY-RUN] Would remove redundant delete-after annotation")
				return ctrl.Result{}, nil
			}
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
			return ctrl.Result{}, nil
		}
		referenceTime := r.getReferenceTime(obj, logger)
		deletionTime := referenceTime.Add(duration)

		if isDryRun {
			logger.Info("[DRY-RUN] Would convert delete-after to delete-at without modifying the resource", "deleteAfter", deleteAfterStr, "calculatedDeleteAt", deletionTime.UTC().Format(time.RFC3339))
			r.Recorder.Eventf(obj, "Normal", "DryRunDelete", "Dry-run: Resource would be scheduled for deletion at %s.", deletionTime.UTC().Format(time.RFC3339))
			return ctrl.Result{}, nil
		}

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
		return ctrl.Result{Requeue: true}, nil
	}

	if deleteAtStr := annotations[DeleteAtAnnotation]; deleteAtStr != "" {
		deleteAtTime, err := time.Parse(time.RFC3339, deleteAtStr)
		if err != nil {
			logger.Error(err, "invalid format for delete-at annotation", "value", deleteAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for delete-at annotation: %v", err)
			return ctrl.Result{}, nil
		}

		if time.Now().After(deleteAtTime) {
			logger.Info("Deleting resource based on delete-at annotation", "targetTime", deleteAtTime.String())
			if !isDryRun {
				if err := r.Delete(ctx, obj); err != nil {
					if !apierrors.IsNotFound(err) {
						logger.Error(err, "failed to delete object")
						r.Recorder.Eventf(obj, "Warning", "DeletionFailed", "Failed to delete resource: %v", err)
						return ctrl.Result{}, err
					}
				}
				r.Recorder.Eventf(obj, "Normal", "Deleted", "Resource deleted based on delete-at annotation: %s", deleteAtStr)
			} else {
				logger.Info("[DRY-RUN] Would delete resource now")
				r.Recorder.Event(obj, "Normal", "DryRunDelete", "Dry-run: Resource would be deleted now.")
			}
			return ctrl.Result{}, nil
		} else {
			requeueAfter := time.Until(deleteAtTime)
			logger.Info("Resource deletion scheduled for a future time", "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	return ctrl.Result{}, nil
}

// handleRestart implements the full restart logic with precedence.
func (r *LifecycleReconciler) handleRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (ctrl.Result, error) {
	_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template")
	if err != nil || !found {
		logger.Info("Resource is not a pod-spawner, skipping restart action.", "resource", client.ObjectKeyFromObject(obj))
		r.Recorder.Event(obj, "Warning", "NotPodSpawner", "Restart annotations are present but resource does not have a spec.template field.")
		return ctrl.Result{}, nil
	}

	annotations := obj.GetAnnotations()
	now := time.Now()

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
			return ctrl.Result{Requeue: true}, nil
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
		return ctrl.Result{Requeue: true}, nil
	}

	if restartAtStr := annotations[RestartAtAnnotation]; restartAtStr != "" {
		restartTime, err := time.Parse(time.RFC3339, restartAtStr)
		if err != nil {
			logger.Error(err, "invalid format for restart-at annotation", "value", restartAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-at annotation: %v", err)
			return ctrl.Result{}, nil
		}
		if now.After(restartTime) {
			logger.Info("Triggering one-time restart based on restart-at annotation", "restartTime", restartTime)
			if err := r.triggerRestart(ctx, obj, isDryRun, func(annotations map[string]string) {
				delete(annotations, RestartAtAnnotation)
			}, logger); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else {
			requeueAfter := time.Until(restartTime)
			logger.Info("One-time restart scheduled for a future time", "requeueAfter", requeueAfter)
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
			return ctrl.Result{}, nil
		}

		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		schedule, err := parser.Parse(fmt.Sprintf("CRON_TZ=%s %s", cronTimezone, cronStr))
		if err != nil {
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			return ctrl.Result{}, nil
		}
		if schedule.Next(time.Now()).IsZero() {
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
	annotations := obj.GetAnnotations()
	now := time.Now()
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
		return requeueForNextOccurrence(schedule.Next(now)), nil
	}

	lastRestartTime, err := time.Parse(time.RFC3339, lastRestartStr)
	if err != nil {
		logger.Error(err, "failed to parse last-restart-timestamp", "value", lastRestartStr)
		r.Recorder.Eventf(obj, "Warning", "InvalidState", "Could not parse last-restart-timestamp: %v", err)
		return ctrl.Result{}, nil
	}

	nextScheduledRestart, coalescedAnchor, restartDue := planRecurringRestart(schedule, lastRestartTime, now)

	if restartDue {
		logger.Info("Triggering recurring restart", "type", scheduleType, "scheduledAt", nextScheduledRestart)
		if err := r.triggerRestart(ctx, obj, isDryRun, func(annotations map[string]string) {
			annotations[LastRestartTimestamp] = coalescedAnchor.UTC().Format(time.RFC3339Nano)
		}, logger); err != nil {
			return ctrl.Result{}, err
		}
		if isDryRun {
			return ctrl.Result{}, nil
		}
		return requeueForNextOccurrence(schedule.Next(coalescedAnchor)), nil
	} else {
		result := requeueForNextOccurrence(nextScheduledRestart)
		logger.Info("Next recurring restart is scheduled", "type", scheduleType, "at", nextScheduledRestart, "requeueAfter", result.RequeueAfter)
		return result, nil
	}
}

// triggerRestart applies the rollout marker and its acknowledgement in one patch.
func (r *LifecycleReconciler) triggerRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, acknowledge func(map[string]string), logger logr.Logger) error {
	restartedAtTime := time.Now().UTC().Format(time.RFC3339)
	logger.Info("Attempting to trigger restart", "restartedAt", restartedAtTime)

	if isDryRun {
		logger.Info("[DRY-RUN] Would trigger restart by setting template annotation", "annotation", RestartedAtTemplate, "value", restartedAtTime)
		r.Recorder.Event(obj, "Normal", "DryRunRestart", "Dry-run: Resource would be restarted now.")
		return nil
	}

	base := obj.DeepCopy()
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	acknowledge(annotations)
	obj.SetAnnotations(annotations)
	markManagedBy(obj)

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

	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, obj, patch); err != nil {
		logger.Error(err, "failed to patch object to trigger restart")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not patch object to trigger restart: %v", err)
		return err
	}

	logger.Info("Successfully patched object to trigger restart")
	r.Recorder.Event(obj, "Normal", "RestartTriggered", "Triggered a rolling restart of the resource.")
	return nil
}
