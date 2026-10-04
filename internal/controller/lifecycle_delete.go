package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *LifecycleReconciler) handleDeletion(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, logger logr.Logger) (result ctrl.Result, err error) {
	defer func() {
		if err == nil && result.IsZero() {
			r.forgetTiming(obj)
		}
	}()
	policy, valid := r.catchUpPolicy(obj, deleteAction, isDryRun)
	if !valid {
		return ctrl.Result{}, nil
	}
	now := r.currentTime()
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
			r.armAbsolute(obj, deleteAction, policy, DeleteAtAnnotation, now)
			return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
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
		r.armAbsolute(obj, deleteAction, policy, DeleteAtAnnotation, now)
		return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
	}

	if deleteAtStr := annotations[DeleteAtAnnotation]; deleteAtStr != "" {
		deleteAtTime, err := time.Parse(time.RFC3339, deleteAtStr)
		if err != nil {
			logger.Error(err, "invalid format for delete-at annotation", "value", deleteAtStr)
			r.Recorder.Eventf(obj, "Warning", "InvalidAnnotation", "Invalid format for delete-at annotation: %v", err)
			return ctrl.Result{}, nil
		}

		allowed := r.observeOccurrence(obj, deleteAction, policy, deleteAtTime, now, isDryRun)
		if !now.Before(deleteAtTime) {
			if !allowed {
				return ctrl.Result{}, r.skipAction(ctx, obj, deleteAction, deleteAtTime, isDryRun, func(a map[string]string) {
					delete(a, DeleteAtAnnotation)
				})
			}
			logger.Info("Deleting resource based on delete-at annotation", "targetTime", deleteAtTime.String())
			if !isDryRun {
				uid := obj.GetUID()
				resourceVersion := obj.GetResourceVersion()
				preconditions := client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}
				if err := r.Delete(ctx, obj, preconditions); err != nil {
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
			result := r.requeueForNextOccurrence(deleteAtTime)
			logger.Info("Resource deletion scheduled for a future time", "requeueAfter", result.RequeueAfter)
			return result, nil
		}
	}

	return ctrl.Result{}, nil
}
