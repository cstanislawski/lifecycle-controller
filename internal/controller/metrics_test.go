package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestLifecycleMetricsRecordActionAndMisconfiguration(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newLifecycleMetrics(registry)
	obj := metricTestObject("metrics-action")

	metrics.recordAction(obj, metricActionDelete, metricResultSuccess)
	metrics.recordAction(obj, metricActionRestart, metricResultDryRun)
	metrics.recordMisconfiguration(obj, "invalid_annotation")

	if got := testutil.ToFloat64(metrics.actionsTotal.WithLabelValues("delete", "Deployment", "default", "success")); got != 1 {
		t.Fatalf("delete success counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.actionsTotal.WithLabelValues("restart", "Deployment", "default", "dry_run")); got != 1 {
		t.Fatalf("restart dry-run counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.misconfigs.WithLabelValues("Deployment", "default", "invalid_annotation")); got != 1 {
		t.Fatalf("misconfiguration counter = %v, want 1", got)
	}
}

func TestLifecycleMetricsNextActionTimestampAggregatesEarliest(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newLifecycleMetrics(registry)
	first := metricTestObject("first-schedule")
	second := metricTestObject("second-schedule")
	firstTime := time.Unix(1_700_000_000, 0).UTC()
	secondTime := firstTime.Add(time.Hour)

	metrics.observeNextAction(second, metricActionDelete, secondTime)
	metrics.observeNextAction(first, metricActionDelete, firstTime)

	if got := testutil.ToFloat64(metrics.nextAction.WithLabelValues("delete", "Deployment", "default")); got != float64(firstTime.Unix()) {
		t.Fatalf("next action timestamp = %v, want %v", got, firstTime.Unix())
	}

	metrics.clearNextAction(first)

	if got := testutil.ToFloat64(metrics.nextAction.WithLabelValues("delete", "Deployment", "default")); got != float64(secondTime.Unix()) {
		t.Fatalf("next action timestamp after clearing first = %v, want %v", got, secondTime.Unix())
	}
}

func TestLifecycleMetricsNextActionTimestampDeletesEmptySeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newLifecycleMetrics(registry)
	obj := metricTestObject("clear-schedule")

	metrics.observeNextAction(obj, metricActionRestart, time.Unix(1_700_000_000, 0).UTC())
	metrics.clearNextAction(obj)

	if err := testutil.GatherAndCompare(registry, strings.NewReader(""), "lifecycle_next_action_timestamp"); err != nil {
		t.Fatalf("expected no next action timestamp series after clear: %v", err)
	}
}

func TestReconcileLogicRecordsNextDeleteMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newLifecycleMetrics(registry)
	reconciler := &LifecycleReconciler{Metrics: metrics}
	obj := metricTestObject("scheduled-delete")
	deleteAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	obj.SetAnnotations(map[string]string{
		DeleteAtAnnotation: deleteAt.Format(time.RFC3339),
	})

	result, err := reconciler.reconcileLogic(context.Background(), obj, logr.Discard())
	if err != nil {
		t.Fatalf("reconcileLogic returned error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive duration", result.RequeueAfter)
	}
	if got := testutil.ToFloat64(metrics.nextAction.WithLabelValues("delete", "Deployment", "default")); got != float64(deleteAt.Unix()) {
		t.Fatalf("next action timestamp = %v, want %v", got, deleteAt.Unix())
	}
}

func metricTestObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	obj.SetNamespace("default")
	obj.SetName(name)
	obj.SetUID(types.UID("default-" + name))
	return obj
}
