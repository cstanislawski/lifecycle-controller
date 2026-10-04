package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// triggerRestart applies the rollout marker and its acknowledgement in one patch.
func (r *LifecycleReconciler) triggerRestart(ctx context.Context, obj *unstructured.Unstructured, isDryRun bool, acknowledge func(map[string]string), logger logr.Logger) error {
	restartedAtTime := r.currentTime().UTC().Format(time.RFC3339)
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
	r.Recorder.Event(obj, "Normal", "RestartTriggered", "Updated Pod template for a restart request")
	return nil
}
