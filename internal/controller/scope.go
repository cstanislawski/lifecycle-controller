package controller

import (
	"fmt"
	"path/filepath"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ScopeConfig holds the patterns for filtering resources and namespaces.
type ScopeConfig struct {
	WatchResources   []string
	IgnoreResources  []string
	WatchNamespaces  []string
	IgnoreNamespaces []string
}

// watchesNamespaceObjects requires an exact selection when namespace filters are set.
func (c ScopeConfig) watchesNamespaceObjects() bool {
	return slices.Contains(c.WatchResources, "namespaces") && c.IsResourceAllowed("namespaces", "")
}

// matches checks if value matches any of the glob patterns.
func matches(patterns []string, value string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, value); matched {
			return true
		}
	}
	return false
}

// IsResourceAllowed determines if a specific GVK should be watched.
// key format expected: "resource.group" (e.g. "deployments.apps", "pods").
// If the group is empty (core), "resource" is sufficient.
func (c *ScopeConfig) IsResourceAllowed(resource, group string) bool {
	key := resource
	if group != "" {
		key = fmt.Sprintf("%s.%s", resource, group)
	}

	// 1. Explicit Ignore takes precedence
	if len(c.IgnoreResources) > 0 && matches(c.IgnoreResources, key) {
		return false
	}

	// 2. Explicit Allow (if defined, only allow matches)
	if len(c.WatchResources) > 0 {
		return matches(c.WatchResources, key)
	}

	// 3. Default Allow (if no allow list defined)
	return true
}

// IsNamespaceAllowed determines if a namespace should be reconciled.
func (c *ScopeConfig) IsNamespaceAllowed(namespace string) bool {
	// 1. Explicit Ignore takes precedence
	if len(c.IgnoreNamespaces) > 0 && matches(c.IgnoreNamespaces, namespace) {
		return false
	}

	// 2. Explicit Allow (if defined, only allow matches)
	if len(c.WatchNamespaces) > 0 {
		return matches(c.WatchNamespaces, namespace)
	}

	// 3. Default Allow
	return true
}

// NamespaceScopePredicate filters events based on the namespace configuration.
// It accepts a provider function to allow dynamic config updates during testing.
func NamespaceScopePredicate(configProvider func() ScopeConfig) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return allow(configProvider(), e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return allow(configProvider(), e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return allow(configProvider(), e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return allow(configProvider(), e.Object)
		},
	}
}

func allow(config ScopeConfig, obj client.Object) bool {
	ns := obj.GetNamespace()

	// Case 1: Resource is inside a namespace
	if ns != "" {
		return config.IsNamespaceAllowed(ns)
	}

	// Case 2: Resource is Cluster-Scoped (ns == "")

	gvk := obj.GetObjectKind().GroupVersionKind()
	_, nativeNamespace := obj.(*corev1.Namespace)
	if nativeNamespace || (gvk.Group == "" && gvk.Version == "v1" && gvk.Kind == "Namespace") {
		if len(config.WatchNamespaces) > 0 && !config.watchesNamespaceObjects() {
			return false
		}
		return config.IsNamespaceAllowed(obj.GetName())
	}

	// Other Cluster-Scoped Resources (Nodes, ClusterRoles, etc.)

	// If the user specifically defined a Watch List, they are opting into strict scoping.
	// "Watch these namespaces" implies "Only watch things belonging to these namespaces".
	// Therefore, generic cluster resources are excluded.
	if len(config.WatchNamespaces) > 0 {
		return false
	}

	// If the user only defined an Ignore List (or nothing), we default to allowing
	// cluster-scoped resources.
	return true
}
