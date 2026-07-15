package controller

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// eventRecorder preserves the controller's existing event call shape while the
// manager uses the structured Kubernetes events API.
type eventRecorder interface {
	Event(object runtime.Object, eventtype, reason, message string)
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})
}

type eventRecorderAdapter struct {
	recorder events.EventRecorder
}

var _ eventRecorder = (*eventRecorderAdapter)(nil)

func newEventRecorderAdapter(recorder events.EventRecorder) eventRecorder {
	return &eventRecorderAdapter{recorder: recorder}
}

func (r *eventRecorderAdapter) Event(object runtime.Object, eventtype, reason, message string) {
	r.recorder.Eventf(object, nil, eventtype, reason, "Reconcile", "%s", message)
}

func (r *eventRecorderAdapter) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.recorder.Eventf(object, nil, eventtype, reason, "Reconcile", messageFmt, args...)
}
