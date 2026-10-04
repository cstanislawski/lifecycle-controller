package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var lifecycleActionAnnotations = []string{
	DeleteAtAnnotation,
	DeleteAfterAnnotation,
	RestartAtAnnotation,
	RestartAfterAnnotation,
	RestartCronAnnotation,
	RestartEveryAnnotation,
}

// LifecyclePredicate limits the shared workqueue to lifecycle-relevant events.
// Restart state changes also enqueue pending actions.
func LifecyclePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasLifecycleAction(e.Object)
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: lifecycleConfigurationChanged,
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func lifecycleConfigurationChanged(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}

	oldAnnotations := e.ObjectOld.GetAnnotations()
	newAnnotations := e.ObjectNew.GetAnnotations()
	oldHasAction := hasLifecycleAction(e.ObjectOld)
	newHasAction := hasLifecycleAction(e.ObjectNew)
	if !oldHasAction && !newHasAction {
		return false
	}

	for _, key := range lifecycleActionAnnotations {
		if oldAnnotations[key] != newAnnotations[key] {
			return true
		}
	}

	if oldAnnotations[DryRunAnnotation] != newAnnotations[DryRunAnnotation] {
		return true
	}
	if oldAnnotations[ReferencePointAnnotation] != newAnnotations[ReferencePointAnnotation] &&
		(hasRelativeAction(oldAnnotations) || hasRelativeAction(newAnnotations)) {
		return true
	}
	if (hasRecurringRestart(oldAnnotations) || hasRecurringRestart(newAnnotations)) &&
		oldAnnotations[LastRestartTimestamp] != newAnnotations[LastRestartTimestamp] {
		return true
	}
	return oldAnnotations[CronTimezoneAnnotation] != newAnnotations[CronTimezoneAnnotation] &&
		(oldAnnotations[RestartCronAnnotation] != "" || newAnnotations[RestartCronAnnotation] != "")
}

func hasLifecycleAction(obj client.Object) bool {
	if obj == nil {
		return false
	}
	annotations := obj.GetAnnotations()
	for _, key := range lifecycleActionAnnotations {
		if annotations[key] != "" {
			return true
		}
	}
	return false
}

func hasRelativeAction(annotations map[string]string) bool {
	return annotations[DeleteAfterAnnotation] != "" || annotations[RestartAfterAnnotation] != ""
}

func hasRecurringRestart(annotations map[string]string) bool {
	return annotations[RestartCronAnnotation] != "" || annotations[RestartEveryAnnotation] != ""
}
