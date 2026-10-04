package controller

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRestartUnsupportedKindsDoNotMutateOrSchedule(t *testing.T) {
	tests := []struct {
		name, group, kind, annotation, value string
	}{
		{"native group, unsupported kind", "apps", "ReplicaSet", RestartAtAnnotation, "2000-01-01T00:00:00Z"},
		{"custom group, native kind", "example.test", "Deployment", RestartAtAnnotation, "2000-01-01T00:00:00Z"},
		{"unsupported relative restart", "example.test", "Deployment", RestartAfterAnnotation, "1s"},
		{"unsupported interval initialization", "example.test", "Deployment", RestartEveryAnnotation, "1m"},
		{"unsupported cron initialization", "example.test", "Deployment", RestartCronAnnotation, "* * * * *"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := testDeployment("unsupported-restart", map[string]string{tt.annotation: tt.value})
			obj.SetGroupVersionKind(schema.GroupVersionKind{Group: tt.group, Version: "v1", Kind: tt.kind})
			setTemplate(t, obj)
			before := obj.DeepCopy()
			tracked := &mutationTrackingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
			recorder := record.NewFakeRecorder(1)
			r := &LifecycleReconciler{Client: tracked, Recorder: recorder}
			result, err := r.reconcileLogic(t.Context(), obj, logr.Discard())
			if err != nil || !result.IsZero() {
				t.Fatalf("unsupported restart: result=%+v error=%v", result, err)
			}
			assertNoTargetMutation(t, before, obj, tracked)
			select {
			case warning := <-recorder.Events:
				if !strings.Contains(warning, "Warning UnsupportedRestartKind") {
					t.Fatalf("event=%q, want UnsupportedRestartKind warning", warning)
				}
			default:
				t.Fatal("unsupported restart has no warning Event")
			}
		})
	}
}
