package controller

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Every watched resource may carry deletion or restart annotations.
var requiredWatchVerbs = sets.New("get", "list", "watch", "patch", "delete")

func planResources(lists []*metav1.APIResourceList, config ScopeConfig, logger logr.Logger) (*resourcePlan, error) {
	plan := &resourcePlan{counts: make(map[[2]string]int)}
	resolvedPatterns := make(map[string]bool, len(config.WatchResources))
	var unwatchable []string

	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			plan.count(discoveryResultFailed, discoveryReasonInvalidGroup)
			publishPlanMetrics(plan)
			logger.Error(err, "Invalid group version returned by discovery", "groupVersion", list.GroupVersion)
			return nil, fmt.Errorf("invalid discovered group version %q: %w", list.GroupVersion, err)
		}

		for _, resource := range list.APIResources {
			key := resourceKey(resource.Name, gv.Group)
			requested := matchingPatterns(config.WatchResources, key)
			ignored := matches(config.IgnoreResources, key)

			if ignored {
				// Ignore precedence removes this match from the required effective watch set.
				for _, pattern := range requested {
					resolvedPatterns[pattern] = true
				}
				plan.skip(logger, key, discoveryReasonConfigured)
				continue
			}
			if len(config.WatchResources) > 0 && len(requested) == 0 {
				plan.skip(logger, key, discoveryReasonConfigured)
				continue
			}
			for _, pattern := range requested {
				resolvedPatterns[pattern] = true
			}
			explicitNamespace := gv.Group == "" && resource.Name == "namespaces" && config.watchesNamespaceObjects()
			if len(config.WatchNamespaces) > 0 && !resource.Namespaced && !explicitNamespace {
				if len(requested) > 0 {
					plan.count(discoveryResultFailed, discoveryReasonRequestUnusable)
					publishPlanMetrics(plan)
					return nil, fmt.Errorf("cluster-scoped resource %s requires an empty namespace watch list; "+
						"only an exact namespaces resource selection can be combined with namespace filters", key)
				}
				plan.skip(logger, key, discoveryReasonConfigured)
				continue
			}
			if strings.Contains(resource.Name, "/") {
				plan.skip(logger, key, discoveryReasonSubresource)
				if len(requested) > 0 {
					unwatchable = append(unwatchable, key)
				}
				continue
			}

			verbs := sets.New(resource.Verbs...)
			if !verbs.HasAll(requiredWatchVerbs.UnsortedList()...) {
				plan.skip(logger, key, discoveryReasonUnsupportedVerbs)
				if len(requested) > 0 {
					unwatchable = append(unwatchable, key)
				}
				continue
			}

			plan.resources = append(plan.resources, discoveredResource{
				key: key,
				gvk: schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: resource.Kind},
			})
			plan.count(discoveryResultDiscovered, discoveryReasonWatchable)
		}
	}

	var missing []string
	ignoredPatterns := sets.New(config.IgnoreResources...)
	for _, pattern := range config.WatchResources {
		if !resolvedPatterns[pattern] && !ignoredPatterns.Has(pattern) {
			missing = append(missing, pattern)
		}
	}
	sort.Strings(missing)
	sort.Strings(unwatchable)
	if len(missing) > 0 || len(unwatchable) > 0 {
		if len(missing) > 0 {
			plan.countBy(discoveryResultFailed, discoveryReasonRequestMissing, len(missing))
			for _, pattern := range missing {
				logger.Error(errors.New("requested resource pattern was not discovered"), "Required resource is missing", "pattern", pattern)
			}
		}
		if len(unwatchable) > 0 {
			plan.countBy(discoveryResultFailed, discoveryReasonRequestUnusable, len(unwatchable))
			for _, key := range unwatchable {
				logger.Error(errors.New("required watch verbs are unavailable"), "Required resource is unwatchable", "resource", key)
			}
		}
		publishPlanMetrics(plan)
		return nil, fmt.Errorf("explicit resource coverage incomplete: missing patterns=%v, unwatchable resources=%v", missing, unwatchable)
	}

	sort.Slice(plan.resources, func(i, j int) bool { return plan.resources[i].key < plan.resources[j].key })
	publishPlanMetrics(plan)
	return plan, nil
}

func resourceKey(resource, group string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}

func matchingPatterns(patterns []string, key string) []string {
	var matched []string
	for _, pattern := range patterns {
		if matches([]string{pattern}, key) {
			matched = append(matched, pattern)
		}
	}
	return matched
}

func (p *resourcePlan) count(result, reason string) {
	p.counts[[2]string{result, reason}]++
}

func (p *resourcePlan) countBy(result, reason string, count int) {
	p.counts[[2]string{result, reason}] += count
}

func (p *resourcePlan) skip(logger logr.Logger, key, reason string) {
	p.count(discoveryResultSkipped, reason)
	logger.V(1).Info("Skipping discovered resource", "resource", key, "reason", reason)
}

func publishPlanMetrics(plan *resourcePlan) {
	resetDiscoveryResourceMetrics()
	for labels, count := range plan.counts {
		setDiscoveryResourceMetric(labels[0], labels[1], count)
	}
}
