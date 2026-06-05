package controller

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
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
	Metrics        *lifecycleMetrics
}

// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;delete;update;patch
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
		r.lifecycleMetrics().clearNextAction(obj)
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
		r.lifecycleMetrics().recordMisconfiguration(obj, "conflicting_annotations")
		r.lifecycleMetrics().clearNextAction(obj)
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
