package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Constants for annotations
const (
	TimezoneAnnotation              = "lifecycle.cezary.dev/timezone"
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
	ReferencePointCreationTimestamp = "creationTimestamp"
)

// LifecycleReconciler reconciles objects with lifecycle annotations.
type LifecycleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=namespaces;pods;services;configmaps;secrets,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// parseExtendedDuration enhances time.ParseDuration to support 'd' for days.
func parseExtendedDuration(durationStr string) (time.Duration, error) {
	// Regex to find number and unit, specifically looking for 'd'
	re := regexp.MustCompile(`(\d+)\s*d`)
	matches := re.FindAllStringSubmatch(durationStr, -1)

	totalHours := 0
	// Replace day components with hour components
	processedStr := durationStr
	for _, match := range matches {
		if len(match) == 2 {
			days, err := strconv.Atoi(match[1])
			if err != nil {
				return 0, fmt.Errorf("invalid number of days: %s", match[1])
			}
			totalHours += days * 24
			processedStr = strings.Replace(processedStr, match[0], "", 1)
		}
	}

	// Add the calculated hours to the string if any were found
	if totalHours > 0 {
		processedStr = fmt.Sprintf("%dh%s", totalHours, processedStr)
	}

	// Remove spaces to avoid parsing issues with the remaining string
	processedStr = strings.ReplaceAll(processedStr, " ", "")

	if processedStr == "" {
		if totalHours > 0 {
			return time.Duration(totalHours) * time.Hour, nil
		}
		return 0, fmt.Errorf("duration string '%s' is empty or invalid", durationStr)
	}

	return time.ParseDuration(processedStr)
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

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var obj client.Object
	potentialTypes := []client.Object{
		&appsv1.Deployment{}, &appsv1.StatefulSet{}, &appsv1.DaemonSet{}, &corev1.Namespace{},
	}
	for _, t := range potentialTypes {
		err := r.Get(ctx, req.NamespacedName, t)
		if err == nil {
			obj = t
			break
		}
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to get object", "type", t.GetObjectKind().GroupVersionKind().Kind)
			return ctrl.Result{}, err
		}
	}
	if obj == nil {
		logger.Info("object not found, likely deleted")
		return ctrl.Result{}, nil
	}
	return r.reconcileLogic(ctx, obj, logger)
}

func (r *LifecycleReconciler) reconcileLogic(ctx context.Context, obj client.Object, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()
	if len(annotations) == 0 {
		return ctrl.Result{}, nil
	}

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		logger.Error(err, "could not convert object to unstructured")
		return ctrl.Result{}, err
	}
	u := &unstructured.Unstructured{Object: unstructuredObj}

	isDryRun := annotations[DryRunAnnotation] == "true"
	timezoneStr := annotations[TimezoneAnnotation]
	if timezoneStr == "" {
		timezoneStr = "UTC"
	}
	location, err := time.LoadLocation(timezoneStr)
	if err != nil {
		logger.Error(err, "invalid timezone specified", "timezone", timezoneStr)
		r.Recorder.Eventf(obj, "Warning", "InvalidTimezone", "Invalid timezone '%s', taking no action.", timezoneStr)
		return ctrl.Result{}, nil
	}

	hasDeleteAnno := annotations[DeleteAtAnnotation] != "" || annotations[DeleteAfterAnnotation] != ""
	hasRestartAnno := annotations[RestartAtAnnotation] != "" || annotations[RestartAfterAnnotation] != "" || annotations[RestartCronAnnotation] != "" || annotations[RestartEveryAnnotation] != ""

	if hasDeleteAnno && hasRestartAnno {
		logger.Info("Conflict: Resource has both delete and restart annotations. Taking no action.", "resource", client.ObjectKeyFromObject(obj))
		r.Recorder.Event(obj, "Warning", "ConflictingAnnotations", "Resource has both delete and restart annotations.")
		return ctrl.Result{}, nil
	}

	if hasDeleteAnno {
		return r.handleDeletion(ctx, u, isDryRun, location, logger)
	}
	if hasRestartAnno {
		return r.handleRestart(ctx, u, isDryRun, location, logger)
	}

	return ctrl.Result{}, nil
}

func (r *LifecycleReconciler) handleDeletion(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, location *time.Location, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()

	if deleteAfterStr := annotations[DeleteAfterAnnotation]; deleteAfterStr != "" {
		if annotations[ReferencePointAnnotation] == ReferencePointCreationTimestamp && annotations[DeleteAtAnnotation] != "" {
			logger.Info("Ignoring delete-after because delete-at is already set with creationTimestamp reference point")
			delete(annotations, DeleteAfterAnnotation)
			obj.SetAnnotations(annotations)
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

		logger.Info("Converting delete-after to delete-at", "deleteAfter", deleteAfterStr, "calculatedDeleteAt", deletionTime.UTC().Format(time.RFC3339))
		newAnnotations := obj.GetAnnotations()
		if newAnnotations == nil {
			newAnnotations = make(map[string]string)
		}
		newAnnotations[DeleteAtAnnotation] = deletionTime.UTC().Format(time.RFC3339)
		delete(newAnnotations, DeleteAfterAnnotation)
		obj.SetAnnotations(newAnnotations)

		if err := r.Update(ctx, obj); err != nil {
			logger.Error(err, "failed to update object with delete-at annotation")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if deleteAtStr := annotations[DeleteAtAnnotation]; deleteAtStr != "" {
		deleteAtTime, err := time.ParseInLocation(time.RFC3339, deleteAtStr, location)
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
func (r *LifecycleReconciler) handleRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, location *time.Location, logger logr.Logger) (ctrl.Result, error) {
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
			delete(annotations, RestartAfterAnnotation)
			obj.SetAnnotations(annotations)
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

		logger.Info("Converting restart-after to restart-at", "restartAfter", restartAfterStr, "calculatedRestartAt", restartTime.UTC().Format(time.RFC3339))
		newAnnotations := obj.GetAnnotations()
		if newAnnotations == nil {
			newAnnotations = make(map[string]string)
		}
		newAnnotations[RestartAtAnnotation] = restartTime.UTC().Format(time.RFC3339)
		delete(newAnnotations, RestartAfterAnnotation)
		obj.SetAnnotations(newAnnotations)
		if err := r.Update(ctx, obj); err != nil {
			logger.Error(err, "failed to update object with restart-at annotation")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if restartAtStr := annotations[RestartAtAnnotation]; restartAtStr != "" {
		restartTime, err := time.ParseInLocation(time.RFC3339, restartAtStr, location)
		if err != nil {
			logger.Error(err, "invalid format for restart-at annotation", "value", restartAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for restart-at annotation: %v", err)
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
				if err := r.Update(ctx, obj); err != nil {
					logger.Error(err, "failed to remove restart-at annotation")
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		} else {
			requeueAfter := time.Until(restartTime)
			logger.Info("One-time restart scheduled for a future time", "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	if cronStr := annotations[RestartCronAnnotation]; cronStr != "" {
		schedule, err := cron.ParseStandard(cronStr)
		if err != nil {
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, location, "cron", schedule, logger)
	}

	if everyStr := annotations[RestartEveryAnnotation]; everyStr != "" {
		duration, err := parseExtendedDuration(everyStr)
		if err != nil {
			logger.Error(err, "invalid duration for restart-every", "duration", everyStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid duration for restart-every: %v", err)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, location, "interval", duration, logger)
	}

	return ctrl.Result{}, nil
}

// reconcileRecurringRestart handles the stateful logic for both cron and interval restarts.
func (r *LifecycleReconciler) reconcileRecurringRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, location *time.Location, scheduleType string, schedule interface{}, logger logr.Logger) (ctrl.Result, error) {
	annotations := obj.GetAnnotations()
	now := time.Now()
	lastRestartStr := annotations[LastRestartTimestamp]

	if lastRestartStr == "" {
		logger.Info("Initializing schedule by setting last-restart-timestamp", "type", scheduleType)
		if !isDryRun {
			annotations[LastRestartTimestamp] = now.In(location).Format(time.RFC3339)
			obj.SetAnnotations(annotations)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to initialize last-restart-timestamp")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, nil
	}

	lastRestartTime, err := time.ParseInLocation(time.RFC3339, lastRestartStr, location)
	if err != nil {
		logger.Error(err, "failed to parse last-restart-timestamp", "value", lastRestartStr)
		r.Recorder.Eventf(obj, "Warning", "InvalidState", "Could not parse last-restart-timestamp: %v", err)
		return ctrl.Result{}, nil
	}

	var nextScheduledRestart time.Time
	if sched, ok := schedule.(cron.Schedule); ok {
		nextScheduledRestart = sched.Next(lastRestartTime)
	} else if duration, ok := schedule.(time.Duration); ok {
		nextScheduledRestart = lastRestartTime.Add(duration)
	}

	if now.After(nextScheduledRestart) {
		logger.Info("Triggering recurring restart", "type", scheduleType, "scheduledAt", nextScheduledRestart)
		if err := r.triggerRestart(ctx, obj, isDryRun, logger); err != nil {
			return ctrl.Result{}, err
		}
		if !isDryRun {
			annotations[LastRestartTimestamp] = nextScheduledRestart.Format(time.RFC3339)
			obj.SetAnnotations(annotations)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to update last-restart-timestamp after restart")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{Requeue: true}, nil
	} else {
		requeueAfter := time.Until(nextScheduledRestart)
		logger.Info("Next recurring restart is scheduled", "type", scheduleType, "at", nextScheduledRestart, "requeueAfter", requeueAfter)
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
	err = unstructured.SetNestedStringMap(obj.Object, templateAnnotations, "spec", "template", "metadata", "annotations")
	if err != nil {
		logger.Error(err, "failed to set restartedAt annotation on pod template")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not set pod template annotations: %v", err)
		return err
	}

	if err := r.Update(ctx, obj); err != nil {
		logger.Error(err, "failed to update object to trigger restart")
		r.Recorder.Eventf(obj, "Warning", "RestartFailed", "Could not update object to trigger restart: %v", err)
		return err
	}

	logger.Info("Successfully updated object to trigger restart")
	r.Recorder.Event(obj, "Normal", "RestartTriggered", "Triggered a rolling restart of the resource.")
	return nil
}

func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("lifecycle-controller")
	annotationPredicate := predicate.AnnotationChangedPredicate{}
	return ctrl.NewControllerManagedBy(mgr).
		Named("lifecycle").
		For(&appsv1.Deployment{}, builder.WithPredicates(annotationPredicate)).
		Watches(&appsv1.StatefulSet{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(annotationPredicate)).
		Watches(&appsv1.DaemonSet{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(annotationPredicate)).
		Watches(&corev1.Namespace{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(annotationPredicate)).
		Complete(r)
}
