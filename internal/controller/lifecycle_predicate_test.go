package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func TestLifecyclePredicateFiltersWorkqueueEvents(t *testing.T) {
	p := LifecyclePredicate()
	actionable := objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h"})
	irrelevant := objectWithAnnotations(map[string]string{"example.com/note": "changed"})

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{name: "create with lifecycle action", got: p.Create(event.CreateEvent{Object: actionable}), want: true},
		{name: "create without lifecycle action", got: p.Create(event.CreateEvent{Object: irrelevant}), want: false},
		{name: "create with empty lifecycle action", got: p.Create(event.CreateEvent{Object: objectWithAnnotations(map[string]string{DeleteAfterAnnotation: ""})}), want: false},
		{name: "delete with lifecycle action", got: p.Delete(event.DeleteEvent{Object: actionable}), want: false},
		{name: "generic with lifecycle action", got: p.Generic(event.GenericEvent{Object: actionable}), want: false},
		{name: "metadata update", got: p.Update(updateEvent(actionable, objectWithNameAndAnnotations("renamed", actionable.GetAnnotations()))), want: false},
		{name: "spec update", got: p.Update(updateEvent(actionable, objectWithSpecAndAnnotations(map[string]interface{}{"replicas": int64(2)}, actionable.GetAnnotations()))), want: false},
		{name: "unrelated annotation update", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", "example.com/note": "changed"}))), want: false},
		{name: "action addition", got: p.Update(updateEvent(objectWithAnnotations(nil), actionable)), want: true},
		{name: "action value update", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "2h"}))), want: true},
		{name: "action removal", got: p.Update(updateEvent(actionable, objectWithAnnotations(nil))), want: true},
		{name: "dry-run update with action", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", DryRunAnnotation: "true"}))), want: true},
		{name: "reference-point update with relative action", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", ReferencePointAnnotation: ReferencePointCreationTimestamp}))), want: true},
		{name: "reference-point update with absolute action", got: p.Update(updateEvent(objectWithAnnotations(map[string]string{DeleteAtAnnotation: "2030-01-01T00:00:00Z"}), objectWithAnnotations(map[string]string{DeleteAtAnnotation: "2030-01-01T00:00:00Z", ReferencePointAnnotation: ReferencePointCreationTimestamp}))), want: false},
		{name: "timezone update with cron action", got: p.Update(updateEvent(objectWithAnnotations(map[string]string{RestartCronAnnotation: "0 * * * *"}), objectWithAnnotations(map[string]string{RestartCronAnnotation: "0 * * * *", CronTimezoneAnnotation: "Europe/Warsaw"}))), want: true},
		{name: "timezone update without cron action", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", CronTimezoneAnnotation: "Europe/Warsaw"}))), want: false},
		{name: "internal last-restart update", got: p.Update(updateEvent(objectWithAnnotations(map[string]string{RestartEveryAnnotation: "1h"}), objectWithAnnotations(map[string]string{RestartEveryAnnotation: "1h", LastRestartTimestamp: "2030-01-01T00:00:00Z"}))), want: false},
		{name: "internal managed-by update", got: p.Update(updateEvent(actionable, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", ManagedByAnnotation: ManagedByValue}))), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("predicate result = %t, want %t", tt.got, tt.want)
			}
		})
	}
}

func TestLifecyclePredicateReducesAcceptedWatchEvents(t *testing.T) {
	oldObject := objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h"})
	events := []event.UpdateEvent{
		updateEvent(oldObject, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "1h", "example.com/note": "changed"})),
		updateEvent(oldObject, objectWithNameAndAnnotations("renamed", oldObject.GetAnnotations())),
		updateEvent(oldObject, objectWithSpecAndAnnotations(map[string]interface{}{"replicas": int64(2)}, oldObject.GetAnnotations())),
		updateEvent(oldObject, objectWithAnnotations(map[string]string{DeleteAfterAnnotation: "2h"})),
	}

	baseline := predicate.AnnotationChangedPredicate{}
	lifecycle := LifecyclePredicate()
	baselineAccepted := 0
	lifecycleAccepted := 0
	createEvent := event.CreateEvent{Object: objectWithAnnotations(nil)}
	deleteEvent := event.DeleteEvent{Object: oldObject}
	genericEvent := event.GenericEvent{Object: oldObject}
	if baseline.Create(createEvent) {
		baselineAccepted++
	}
	if baseline.Delete(deleteEvent) {
		baselineAccepted++
	}
	if baseline.Generic(genericEvent) {
		baselineAccepted++
	}
	if lifecycle.Create(createEvent) {
		lifecycleAccepted++
	}
	if lifecycle.Delete(deleteEvent) {
		lifecycleAccepted++
	}
	if lifecycle.Generic(genericEvent) {
		lifecycleAccepted++
	}
	for _, e := range events {
		if baseline.Update(e) {
			baselineAccepted++
		}
		if lifecycle.Update(e) {
			lifecycleAccepted++
		}
	}

	if lifecycleAccepted != 1 {
		t.Fatalf("lifecycle predicate accepted %d of 7 events, want 1", lifecycleAccepted)
	}
	if baselineAccepted != 5 {
		t.Fatalf("baseline predicate accepted %d of 7 events, want 5", baselineAccepted)
	}
}

func TestScheduledLifecycleActionReturnsRequeueAfter(t *testing.T) {
	reconcileAt := time.Now().Add(time.Hour).UTC()
	obj := objectWithAnnotations(map[string]string{DeleteAtAnnotation: reconcileAt.Format(time.RFC3339)})

	result, err := (&LifecycleReconciler{}).reconcileLogic(t.Context(), obj, logr.Discard())
	if err != nil {
		t.Fatalf("reconcileLogic() error = %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Hour {
		t.Fatalf("RequeueAfter = %s, want a positive duration no greater than 1h", result.RequeueAfter)
	}
}

func updateEvent(oldObject, newObject *unstructured.Unstructured) event.UpdateEvent {
	return event.UpdateEvent{ObjectOld: oldObject, ObjectNew: newObject}
}

func objectWithAnnotations(annotations map[string]string) *unstructured.Unstructured {
	return objectWithNameAndAnnotations("test-object", annotations)
}

func objectWithNameAndAnnotations(name string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetName(name)
	obj.SetAnnotations(annotations)
	return obj
}

func objectWithSpecAndAnnotations(spec map[string]interface{}, annotations map[string]string) *unstructured.Unstructured {
	obj := objectWithAnnotations(annotations)
	obj.Object["spec"] = spec
	return obj
}
