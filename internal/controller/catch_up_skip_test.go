package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func catchUpSkipObject(t *testing.T, action string) *unstructured.Unstructured {
	t.Helper()
	annotations := map[string]string{}
	if action == RestartEveryAnnotation || action == RestartCronAnnotation {
		annotations[action] = "1h"
		if action == RestartCronAnnotation {
			annotations[action] = "0 * * * *"
		}
		annotations[LastRestartTimestamp] = catchUpTestStart.Format(time.RFC3339)
	} else {
		annotations[action] = catchUpTestStart.Add(time.Minute).Format(time.RFC3339)
	}
	obj := testDeployment("skip-action", annotations)
	setTemplate(t, obj)
	return obj
}

func TestCatchUpSkipConflictPreservesConcurrentSchedule(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation, RestartEveryAnnotation, RestartCronAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart.Add(3*time.Hour + time.Minute)
			obj := catchUpSkipObject(t, action)
			changedValue := now.Add(time.Hour).Format(time.RFC3339)
			switch action {
			case RestartEveryAnnotation:
				changedValue = "2h"
			case RestartCronAnnotation:
				changedValue = "0 */2 * * *"
			}
			inject := true
			funcs := interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, target client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if inject {
						inject = false
						current := obj.DeepCopy()
						if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
							return err
						}
						annotations := current.GetAnnotations()
						annotations[action] = changedValue
						annotations["example.com/concurrent"] = "preserved"
						current.SetAnnotations(annotations)
						if err := c.Update(ctx, current); err != nil {
							return err
						}
					}
					return c.Patch(ctx, target, patch, opts...)
				},
			}
			r := catchUpNeverReconciler(t, obj, &now, funcs)
			_, err := r.Reconcile(context.Background(), timingRequest(obj))
			if !apierrors.IsConflict(err) {
				t.Fatalf("skip patch error = %v, want conflict", err)
			}
			current := catchUpFetch(t, r, obj)
			if got := current.GetAnnotations()[action]; got != changedValue {
				t.Fatalf("schedule after conflict = %q, want %q", got, changedValue)
			}
			if got := current.GetAnnotations()["example.com/concurrent"]; got != "preserved" {
				t.Fatalf("concurrent annotation = %q, want preserved", got)
			}
			if got := restartedAt(t, current); got != "" {
				t.Fatalf("skip conflict changed template: %q", got)
			}
			if action == RestartEveryAnnotation || action == RestartCronAnnotation {
				if got := current.GetAnnotations()[LastRestartTimestamp]; got != catchUpTestStart.Format(time.RFC3339) {
					t.Fatalf("conflicted skip changed anchor: %q", got)
				}
			}
		})
	}
}

func TestCatchUpCommittedSkipIsNotReplayedAfterLostResponse(t *testing.T) {
	for _, action := range []string{DeleteAtAnnotation, RestartAtAnnotation, RestartEveryAnnotation, RestartCronAnnotation} {
		t.Run(action, func(t *testing.T) {
			now := catchUpTestStart.Add(3*time.Hour + time.Minute)
			obj := catchUpSkipObject(t, action)
			lostResponse := errors.New("response lost after skip committed")
			patches := 0
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, target client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					if err := c.Patch(ctx, target, patch, opts...); err != nil {
						return err
					}
					return lostResponse
				},
			})
			_, err := r.Reconcile(context.Background(), timingRequest(obj))
			if !errors.Is(err, lostResponse) {
				t.Fatalf("skip patch error = %v, want lost response", err)
			}
			catchUpReconcile(t, r, obj)
			if patches != 1 {
				t.Fatalf("skip patch calls = %d, want 1", patches)
			}
			current := catchUpFetch(t, r, obj)
			if action == RestartEveryAnnotation || action == RestartCronAnnotation {
				wantAnchor := catchUpTestStart.Add(3 * time.Hour).Format(time.RFC3339)
				if action == RestartCronAnnotation {
					wantAnchor = now.Format(time.RFC3339)
				}
				if got := current.GetAnnotations()[LastRestartTimestamp]; got != wantAnchor {
					t.Fatalf("skip anchor = %q, want %q", got, wantAnchor)
				}
				if got := restartedAt(t, current); got != "" {
					t.Fatalf("committed skip changed template: %q", got)
				}
			} else {
				catchUpAssertSkipped(t, r, obj, action)
			}
		})
	}
}

func TestCatchUpNeverBlockedConfigurationClearsArm(t *testing.T) {
	for _, blocked := range []string{"policy", "deadline", "dry-run", "conflict", "no action", "no annotations"} {
		t.Run(blocked, func(t *testing.T) {
			now := catchUpTestStart
			due := now.Add(time.Minute)
			obj := testDeployment("blocked-configuration", map[string]string{RestartAtAnnotation: due.Format(time.RFC3339)})
			setTemplate(t, obj)
			r := catchUpNeverReconciler(t, obj, &now, interceptor.Funcs{})
			catchUpReconcile(t, r, obj)
			catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
				annotations := current.GetAnnotations()
				switch blocked {
				case "policy":
					annotations[CatchUpAnnotation] = "invalid"
				case "deadline":
					annotations[RestartAtAnnotation] = "invalid"
				case "dry-run":
					annotations[DryRunAnnotation] = "invalid"
				case "conflict":
					annotations[DeleteAtAnnotation] = due.Format(time.RFC3339)
				case "no action":
					delete(annotations, RestartAtAnnotation)
					annotations["example.com/other"] = "retained"
				case "no annotations":
					annotations = nil
				}
				current.SetAnnotations(annotations)
			})
			catchUpReconcile(t, r, obj)
			catchUpUpdate(t, r, obj, func(current *unstructured.Unstructured) {
				current.SetAnnotations(map[string]string{RestartAtAnnotation: due.Format(time.RFC3339)})
			})
			now = due.Add(time.Second)
			catchUpReconcile(t, r, obj)
			catchUpAssertSkipped(t, r, obj, RestartAtAnnotation)
		})
	}
}

func TestCatchUpNeverConcurrentResourcesExecuteArmedActions(t *testing.T) {
	now := catchUpTestStart
	due := now.Add(time.Minute)
	const count = 32
	objects := make([]*unstructured.Unstructured, count)
	stored := make([]client.Object, count)
	for i := range objects {
		action := RestartAtAnnotation
		if i%2 == 0 {
			action = DeleteAtAnnotation
		}
		objects[i] = testDeployment(fmt.Sprintf("concurrent-action-%d", i), map[string]string{action: due.Format(time.RFC3339)})
		setTemplate(t, objects[i])
		stored[i] = objects[i].DeepCopy()
	}
	r := catchUpNeverReconciler(t, objects[0], &now, interceptor.Funcs{})
	r.Client = fake.NewClientBuilder().WithObjects(stored...).Build()
	reconcileAll := func() {
		var group sync.WaitGroup
		for _, obj := range objects {
			group.Go(func() {
				if _, err := r.Reconcile(context.Background(), timingRequest(obj)); err != nil {
					t.Errorf("reconcile %s: %v", obj.GetName(), err)
				}
			})
		}
		group.Wait()
	}
	reconcileAll()
	now = due.Add(time.Millisecond)
	reconcileAll()
	for i, obj := range objects {
		action := RestartAtAnnotation
		if i%2 == 0 {
			action = DeleteAtAnnotation
		}
		catchUpAssertExecuted(t, r, obj, action)
	}
}
