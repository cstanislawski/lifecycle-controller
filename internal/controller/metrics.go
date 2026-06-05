package controller

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricActionDelete  = "delete"
	metricActionRestart = "restart"

	metricResultDryRun  = "dry_run"
	metricResultError   = "error"
	metricResultSuccess = "success"
)

type nextActionLabels struct {
	action    string
	kind      string
	namespace string
}

type nextActionObservation struct {
	labels    nextActionLabels
	timestamp float64
}

type lifecycleMetrics struct {
	actionsTotal *prometheus.CounterVec
	nextAction   *prometheus.GaugeVec
	misconfigs   *prometheus.CounterVec

	mu          sync.Mutex
	nextActions map[string]nextActionObservation
}

func newLifecycleMetrics(registerer prometheus.Registerer) *lifecycleMetrics {
	m := &lifecycleMetrics{
		actionsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "lifecycle_actions_total",
				Help: "Total number of lifecycle actions handled by the controller.",
			},
			[]string{"action", "kind", "namespace", "result"},
		),
		nextAction: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "lifecycle_next_action_timestamp",
				Help: "Unix timestamp for the next known lifecycle action, aggregated by action, kind, and namespace.",
			},
			[]string{"action", "kind", "namespace"},
		),
		misconfigs: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "lifecycle_misconfigurations_total",
				Help: "Total number of lifecycle annotation misconfigurations observed by the controller.",
			},
			[]string{"kind", "namespace", "reason"},
		),
		nextActions: make(map[string]nextActionObservation),
	}
	registerer.MustRegister(m.actionsTotal, m.nextAction, m.misconfigs)
	return m
}

var defaultLifecycleMetrics = newLifecycleMetrics(metrics.Registry)

func (r *LifecycleReconciler) lifecycleMetrics() *lifecycleMetrics {
	if r.Metrics != nil {
		return r.Metrics
	}
	return defaultLifecycleMetrics
}

func (m *lifecycleMetrics) recordAction(obj client.Object, action, result string) {
	kind, namespace := metricKindNamespace(obj)
	m.actionsTotal.WithLabelValues(action, kind, namespace, result).Inc()
}

func (m *lifecycleMetrics) recordMisconfiguration(obj client.Object, reason string) {
	kind, namespace := metricKindNamespace(obj)
	m.misconfigs.WithLabelValues(kind, namespace, reason).Inc()
}

func (m *lifecycleMetrics) observeNextAction(obj client.Object, action string, when time.Time) {
	if when.IsZero() {
		m.clearNextAction(obj)
		return
	}

	kind, namespace := metricKindNamespace(obj)
	labels := nextActionLabels{action: action, kind: kind, namespace: namespace}
	key := metricObjectKey(obj)
	observation := nextActionObservation{
		labels:    labels,
		timestamp: float64(when.Unix()),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, hadPrevious := m.nextActions[key]
	m.nextActions[key] = observation
	if hadPrevious && previous.labels != labels {
		m.updateNextActionGaugeLocked(previous.labels)
	}
	m.updateNextActionGaugeLocked(labels)
}

func (m *lifecycleMetrics) clearNextAction(obj client.Object) {
	key := metricObjectKey(obj)

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, ok := m.nextActions[key]
	if !ok {
		return
	}
	delete(m.nextActions, key)
	m.updateNextActionGaugeLocked(previous.labels)
}

func (m *lifecycleMetrics) updateNextActionGaugeLocked(labels nextActionLabels) {
	earliest := math.Inf(1)
	for _, observation := range m.nextActions {
		if observation.labels == labels && observation.timestamp < earliest {
			earliest = observation.timestamp
		}
	}

	if math.IsInf(earliest, 1) {
		m.nextAction.DeleteLabelValues(labels.action, labels.kind, labels.namespace)
		return
	}
	m.nextAction.WithLabelValues(labels.action, labels.kind, labels.namespace).Set(earliest)
}

func metricKindNamespace(obj client.Object) (string, string) {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		kind = "Unknown"
	}
	return kind, obj.GetNamespace()
}

func metricObjectKey(obj client.Object) string {
	kind := obj.GetObjectKind().GroupVersionKind().String()
	if kind == "" {
		kind = "unknown"
	}
	uid := obj.GetUID()
	if uid != "" {
		return fmt.Sprintf("%s/%s", kind, uid)
	}
	return fmt.Sprintf("%s/%s/%s", kind, obj.GetNamespace(), obj.GetName())
}

func nextRestartTime(scheduleType string, schedule interface{}, after time.Time) time.Time {
	switch scheduleType {
	case "cron":
		if sched, ok := schedule.(interface{ Next(time.Time) time.Time }); ok {
			return sched.Next(after)
		}
	case "interval":
		if duration, ok := schedule.(time.Duration); ok {
			return after.Add(duration)
		}
	}
	return time.Time{}
}
