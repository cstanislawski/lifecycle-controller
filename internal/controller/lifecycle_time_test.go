package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOneTimeDeadlineCrossingRetainsTimer(t *testing.T) {
	for _, action := range []struct{ name, at string }{
		{name: "delete", at: DeleteAtAnnotation},
		{name: "restart", at: RestartAtAnnotation},
	} {
		t.Run(action.name, func(t *testing.T) {
			deadline := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
			obj := testDeployment("crossing-"+action.name, map[string]string{action.at: deadline.Format(time.RFC3339)})
			setTemplate(t, obj)
			tracked := &mutationTrackingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
			reads := 0
			r := &LifecycleReconciler{
				Client: tracked, Recorder: record.NewFakeRecorder(8),
				now: func() time.Time {
					reads++
					if reads == 1 {
						return deadline.Add(-time.Nanosecond)
					}
					return deadline.Add(time.Nanosecond)
				},
			}
			result, err := r.reconcileLogic(t.Context(), obj, logr.Discard())
			if err != nil || result.RequeueAfter != time.Nanosecond {
				t.Fatalf("crossed deadline: result=%+v error=%v, want positive immediate timer", result, err)
			}
			if tracked.deletes != 0 || tracked.patches != 0 {
				t.Fatal("future branch ran the action")
			}
			if _, err := r.reconcileLogic(t.Context(), obj, logr.Discard()); err != nil {
				t.Fatal(err)
			}
			if tracked.deletes+tracked.patches != 1 {
				t.Fatalf("after timer: deletes=%d patches=%d, want one action", tracked.deletes, tracked.patches)
			}
		})
	}
}
