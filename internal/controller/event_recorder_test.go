package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

type recordedEvent struct {
	regarding runtime.Object
	related   runtime.Object
	eventType string
	reason    string
	action    string
	note      string
	args      []interface{}
}

type capturingEventRecorder struct {
	events []recordedEvent
}

func (r *capturingEventRecorder) Eventf(regarding, related runtime.Object, eventType, reason, action, note string, args ...interface{}) {
	r.events = append(r.events, recordedEvent{
		regarding: regarding,
		related:   related,
		eventType: eventType,
		reason:    reason,
		action:    action,
		note:      note,
		args:      args,
	})
}

func TestEventRecorderAdapter(t *testing.T) {
	regarding := &runtime.Unknown{}
	capturing := &capturingEventRecorder{}
	adapter := newEventRecorderAdapter(capturing)

	adapter.Event(regarding, "Warning", "InvalidAnnotation", "invalid 50% value")
	adapter.Eventf(regarding, "Normal", "Restarted", "restart %s", "deployment")

	if len(capturing.events) != 2 {
		t.Fatalf("recorded events = %d, want 2", len(capturing.events))
	}

	first := capturing.events[0]
	if first.regarding != regarding || first.related != nil || first.eventType != "Warning" || first.reason != "InvalidAnnotation" || first.action != "Reconcile" || first.note != "%s" || len(first.args) != 1 || first.args[0] != "invalid 50% value" {
		t.Fatalf("first event = %#v", first)
	}

	second := capturing.events[1]
	if second.regarding != regarding || second.related != nil || second.eventType != "Normal" || second.reason != "Restarted" || second.action != "Reconcile" || second.note != "restart %s" || len(second.args) != 1 || second.args[0] != "deployment" {
		t.Fatalf("second event = %#v", second)
	}
}
