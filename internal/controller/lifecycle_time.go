package controller

import (
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *LifecycleReconciler) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *LifecycleReconciler) requeueForNextOccurrence(next time.Time) ctrl.Result {
	if next.IsZero() {
		return ctrl.Result{}
	}

	delay := next.Sub(r.currentTime())
	if delay <= 0 {
		// Controller-runtime discards non-positive timer delays.
		delay = time.Nanosecond
	}
	return ctrl.Result{RequeueAfter: delay}
}
