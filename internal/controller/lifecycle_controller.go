package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
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
	CatchUpAnnotation               = "lifecycle.cezary.dev/catch-up"
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

// LifecycleReconciler reconciles objects with lifecycle annotations.
type LifecycleReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     eventRecorder
	Config       ScopeConfig
	GlobalDryRun bool
	ActionTiming ActionTimingConfig
	now          func() time.Time
	timers       actionTimers
	discovery    preferredResourceDiscovery
	coverage     *coverageState
}

// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;delete;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

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
		return r.currentTime().UTC()
	default:
		logger.Info("Invalid reference-point specified, falling back to 'applyTimestamp'", "value", referencePoint)
		r.Recorder.Eventf(obj, "Warning", "InvalidAnnotationValue", "Invalid value for %s: '%s', falling back to 'applyTimestamp'", ReferencePointAnnotation, referencePoint)
		return r.currentTime().UTC()
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
			r.forgetRequestTiming(req)
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
		r.forgetTiming(obj)
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
		r.forgetTiming(obj)
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
		r.forgetTiming(obj)
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

	r.forgetTiming(obj)
	return ctrl.Result{}, nil
}
