package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	discoveryResultDiscovered = "discovered"
	discoveryResultSkipped    = "skipped"
	discoveryResultFailed     = "failed"

	discoveryReasonWatchable        = "watchable"
	discoveryReasonConfigured       = "configured"
	discoveryReasonSubresource      = "subresource"
	discoveryReasonUnsupportedVerbs = "unsupported_verbs"
	discoveryReasonRequestMissing   = "request_missing"
	discoveryReasonRequestUnusable  = "request_unwatchable"
	discoveryReasonGroupDiscovery   = "group_discovery"
	discoveryReasonInvalidGroup     = "invalid_group_version"
	discoveryReasonCacheSync        = "cache_sync"
)

var (
	discoveryResources = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lifecycle_controller_discovery_resources",
		Help: "Resources observed during the latest discovery attempt, grouped by bounded result and reason.",
	}, []string{"result", "reason"})
	discoveryReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lifecycle_controller_discovery_ready",
		Help: "Whether complete resource discovery and all required watch caches are synchronized on this instance.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(discoveryResources, discoveryReady)
}

func resetDiscoveryResourceMetrics() {
	discoveryResources.Reset()
}

func setDiscoveryResourceMetric(result, reason string, count int) {
	discoveryResources.WithLabelValues(result, reason).Set(float64(count))
}
