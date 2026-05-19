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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
)

type MaxTTLExceededAction string

const (
	MaxTTLExceededReject MaxTTLExceededAction = "reject"
	MaxTTLExceededWarn   MaxTTLExceededAction = "warn"
	MaxTTLExceededIgnore MaxTTLExceededAction = "ignore"
	MaxTTLExceededClamp  MaxTTLExceededAction = "clamp"
)

func IsValidMaxTTLExceededAction(action string) bool {
	switch MaxTTLExceededAction(action) {
	case MaxTTLExceededReject, MaxTTLExceededWarn, MaxTTLExceededIgnore, MaxTTLExceededClamp:
		return true
	default:
		return false
	}
}

// ResourceScope holds the GVK and scope information for a discovered resource.
type ResourceScope struct {
	GVK          schema.GroupVersionKind
	IsNamespaced bool
}

// LifecycleReconciler reconciles objects with lifecycle annotations.
type LifecycleReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	KnownResources []ResourceScope
	Config         ScopeConfig
	GlobalDryRun   bool
	MaxTTL         time.Duration
	MaxTTLExceeded MaxTTLExceededAction
}

// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// ParseExtendedDuration enhances time.ParseDuration to support 'd' for days.
func ParseExtendedDuration(durationStr string) (time.Duration, error) {
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

func markManagedBy(obj client.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[ManagedByAnnotation] = ManagedByValue
	obj.SetAnnotations(annotations)
}

func (r *LifecycleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var foundObject client.Object
	keyIsNamespaced := req.Namespace != ""

	for _, res := range r.KnownResources {
		// Skip resources where the scope does not match the request key's scope.
		// This prevents trying to fetch a namespaced resource with a cluster-scoped key, and vice-versa.
		if res.IsNamespaced != keyIsNamespaced {
			continue
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(res.GVK)
		err := r.Get(ctx, req.NamespacedName, obj)

		if err == nil {
			foundObject = obj
			break
		}

		if !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to get object", "gvk", res.GVK)
			return ctrl.Result{}, err
		}
	}

	if foundObject == nil {
		logger.Info("object not found in any of the watched GVKs, likely deleted")
		return ctrl.Result{}, nil
	}

	return r.reconcileLogic(ctx, foundObject, logger)
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

	isDryRun := r.GlobalDryRun || annotations[DryRunAnnotation] == "true"
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
			delete(annotations, DeleteAfterAnnotation)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to remove redundant delete-after annotation")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		duration, err := ParseExtendedDuration(deleteAfterStr)
		if err != nil {
			logger.Error(err, "invalid duration format for delete-after annotation", "value", deleteAfterStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for delete-after annotation: %v", err)
			return ctrl.Result{}, nil
		}
		duration, ok := r.applyMaxTTLPolicy(obj, DeleteAfterAnnotation, deleteAfterStr, duration, logger)
		if !ok {
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

func (r *LifecycleReconciler) applyMaxTTLPolicy(obj client.Object, annotation, value string, duration time.Duration, logger logr.Logger) (time.Duration, bool) {
	if r.MaxTTL <= 0 || duration <= r.MaxTTL {
		return duration, true
	}

	action := r.MaxTTLExceeded
	if action == "" {
		action = MaxTTLExceededReject
	}

	message := fmt.Sprintf("%s duration %s exceeds configured max TTL %s", annotation, value, r.MaxTTL)
	switch action {
	case MaxTTLExceededWarn:
		logger.Info("Max TTL exceeded; accepting annotation", "annotation", annotation, "value", value, "duration", duration, "maxTTL", r.MaxTTL)
		r.Recorder.Event(obj, "Warning", "MaxTTLExceeded", message)
		return duration, true
	case MaxTTLExceededIgnore:
		logger.Info("Max TTL exceeded; ignoring annotation", "annotation", annotation, "value", value, "duration", duration, "maxTTL", r.MaxTTL)
		r.Recorder.Event(obj, "Warning", "MaxTTLExceeded", message+"; ignoring annotation")
		return 0, false
	case MaxTTLExceededClamp:
		logger.Info("Max TTL exceeded; clamping duration", "annotation", annotation, "value", value, "duration", duration, "maxTTL", r.MaxTTL)
		r.Recorder.Event(obj, "Warning", "MaxTTLExceeded", message+"; clamping to max TTL")
		return r.MaxTTL, true
	case MaxTTLExceededReject:
		logger.Info("Max TTL exceeded; rejecting annotation", "annotation", annotation, "value", value, "duration", duration, "maxTTL", r.MaxTTL)
		r.Recorder.Event(obj, "Warning", "MaxTTLExceeded", message+"; rejecting annotation")
		return 0, false
	default:
		logger.Info("Invalid max TTL exceeded action; rejecting annotation", "action", action)
		r.Recorder.Eventf(obj, "Warning", "InvalidConfiguration", "Invalid max TTL exceeded action %q; rejecting %s", action, annotation)
		return 0, false
	}
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
			delete(annotations, RestartAfterAnnotation)
			obj.SetAnnotations(annotations)
			markManagedBy(obj)
			if err := r.Update(ctx, obj); err != nil {
				logger.Error(err, "failed to remove redundant restart-after annotation")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}

		duration, err := ParseExtendedDuration(restartAfterStr)
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

		schedule, err := cron.ParseStandard(fmt.Sprintf("CRON_TZ=%s %s", cronTimezone, cronStr))
		if err != nil {
			logger.Error(err, "invalid cron expression", "cron", cronStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid cron expression for restart-cron: %v", err)
			return ctrl.Result{}, nil
		}
		return r.reconcileRecurringRestart(ctx, obj, isDryRun, "cron", schedule, logger)
	}

	if everyStr := annotations[RestartEveryAnnotation]; everyStr != "" {
		duration, err := ParseExtendedDuration(everyStr)
		if err != nil {
			logger.Error(err, "invalid duration for restart-every", "duration", everyStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid duration for restart-every: %v", err)
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
		return ctrl.Result{Requeue: true}, nil
	}

	lastRestartTime, err := time.Parse(time.RFC3339, lastRestartStr)
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
			markManagedBy(obj)
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
		return err
	}

	logger.Info("Successfully updated object to trigger restart")
	r.Recorder.Event(obj, "Normal", "RestartTriggered", "Triggered a rolling restart of the resource.")
	return nil
}

func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("lifecycle-controller")

	config := mgr.GetConfig()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}

	// Get the list of all server-preferred API resources
	apiResourceLists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		log.Log.Error(err, "failed to get all server preferred resources, continuing with available ones")
	}

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).Named("lifecycle")
	annotationPredicate := predicate.AnnotationChangedPredicate{}
	// Pass a closure to allow dynamic config updates during tests
	namespacePredicate := NamespaceScopePredicate(func() ScopeConfig {
		return r.Config
	})
	setupLog := mgr.GetLogger().WithName("lifecycle-setup")

	// Dynamically watch all resources that support the necessary verbs
	for _, apiResourceList := range apiResourceLists {
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			setupLog.Error(err, "failed to parse group version", "groupVersion", apiResourceList.GroupVersion)
			continue
		}

		for _, resource := range apiResourceList.APIResources {
			// Filter out subresources (like /status, /scale)
			if strings.Contains(resource.Name, "/") {
				continue
			}

			// Filter resources that don't support the verbs we need
			verbs := sets.NewString(resource.Verbs...)
			if !verbs.HasAll("get", "list", "watch", "patch", "delete") {
				continue
			}

			// Filter based on config
			if !r.Config.IsResourceAllowed(resource.Name, gv.Group) {
				setupLog.V(1).Info("Skipping resource due to configuration", "resource", resource.Name, "group", gv.Group)
				continue
			}

			// Add the GVK and its scope to our list for the Reconcile function
			r.KnownResources = append(r.KnownResources, ResourceScope{
				GVK: schema.GroupVersionKind{
					Group:   gv.Group,
					Version: gv.Version,
					Kind:    resource.Kind,
				},
				IsNamespaced: resource.Namespaced,
			})

			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(r.KnownResources[len(r.KnownResources)-1].GVK)

			setupLog.Info("Setting up watch for resource", "gvk", u.GroupVersionKind().String())
			controllerBuilder = controllerBuilder.Watches(
				u,
				handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, a client.Object) []reconcile.Request {
					return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(a)}}
				}),
				builder.WithPredicates(annotationPredicate, namespacePredicate),
			)
		}
	}

	return controllerBuilder.Complete(r)
}
