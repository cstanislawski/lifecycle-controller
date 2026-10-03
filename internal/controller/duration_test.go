package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExtendedDurationRejectsInvalidInput(t *testing.T) {
	values := []string{
		"", "0", "0s", "0d", "-1h", "+1h", "-1d2h", "+1d",
		"1d-25h", "1d-24h", "1d-1h", "1d+1h", "1h-1d",
		"1.5d", "1.5d2h", ".5d2h", "1h2", "1dgarbage", "1w",
		"1d2h", "2h30m", "1d0.5h", "0d1h", "1d2d", "1h0s",
		" 1d", "1d ", "1 d", "1d 2h", "1\th", "1\nd", "1e3s",
		"768614336404564651d", "106752d", "2562048h",
		"106751d24h", "106751d1d", "9223372037s",
		"500ms", "1us", "1µs", "1μs", "1ns",
		"1.5s", "1.5m", "1.5h", ".5s", ".5h", "1.h",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			if duration, err := parseExtendedDuration(value); err == nil {
				t.Fatalf("duration %q accepted as %s", value, duration)
			}
		})
	}
}

func TestExtendedDurationAcceptsPositiveInput(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{value: "1d", want: 24 * time.Hour},
		{value: "25h", want: 25 * time.Hour},
		{value: "5m", want: 5 * time.Minute},
		{value: "106751d", want: 106751 * 24 * time.Hour},
		{value: "9223372036s", want: 9223372036 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			duration, err := parseExtendedDuration(test.value)
			if err != nil || duration != test.want {
				t.Fatalf("duration %q = %s, %v; want %s", test.value, duration, err, test.want)
			}
		})
	}
}

func TestInvalidRelativeDurationDoesNotAct(t *testing.T) {
	values := []string{"1d-25h", "1d-24h", "1d-1h", "1d+1h", "-1d2h", "1.5d2h", "1d2h", "2h30m", "0s", "60000ms", "1.5h"}
	for _, annotation := range []string{DeleteAfterAnnotation, RestartAfterAnnotation, RestartEveryAnnotation} {
		for _, value := range values {
			t.Run(annotation+"/"+value, func(t *testing.T) {
				obj := testDeployment("invalid-duration", map[string]string{annotation: value})
				setTemplate(t, obj)
				before := obj.DeepCopy()
				tracked := &mutationTrackingClient{Client: fake.NewClientBuilder().WithObjects(obj.DeepCopy()).Build()}
				recorder := record.NewFakeRecorder(2)
				r := &LifecycleReconciler{Client: tracked, Recorder: recorder}

				result, err := r.reconcileLogic(t.Context(), obj, logr.Discard())
				if err != nil || !result.IsZero() {
					t.Fatalf("reconcile = %+v, %v; want no action or retry", result, err)
				}
				assertNoTargetMutation(t, before, obj, tracked)
				stored := getObject(t, r, client.ObjectKeyFromObject(obj))
				if !reflect.DeepEqual(before.GetAnnotations(), stored.GetAnnotations()) {
					t.Fatalf("stored annotations changed: %v", stored.GetAnnotations())
				}
				select {
				case event := <-recorder.Events:
					if !strings.Contains(event, "Warning InvalidAnnotation") {
						t.Fatalf("event = %q; want InvalidAnnotation warning", event)
					}
				default:
					t.Fatal("missing InvalidAnnotation warning")
				}
			})
		}
	}
}
