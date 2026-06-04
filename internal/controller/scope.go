package controller

import (
	"fmt"
	"path/filepath"

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

var defaultIgnoreResources = []string{
	"secrets",
	"serviceaccounts",
	"roles.rbac.authorization.k8s.io",
	"rolebindings.rbac.authorization.k8s.io",
	"clusterroles.rbac.authorization.k8s.io",
	"clusterrolebindings.rbac.authorization.k8s.io",
	"nodes",
	"persistentvolumes",
	"storageclasses.storage.k8s.io",
	"customresourcedefinitions.apiextensions.k8s.io",
}

// DefaultIgnoreResources returns resource patterns ignored when no resource
// allowlist or ignore list is configured.
func DefaultIgnoreResources() []string {
	return append([]string(nil), defaultIgnoreResources...)
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

func (c *ScopeConfig) effectiveIgnoreResources() []string {
	if len(c.WatchResources) == 0 && len(c.IgnoreResources) == 0 {
		return defaultIgnoreResources
	}
	return c.IgnoreResources
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
	ignoreResources := c.effectiveIgnoreResources()
	if len(ignoreResources) > 0 && matches(ignoreResources, key) {
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

	// Special Handling: The "Namespace" resource itself.
	// If we are watching namespace "foo", we want to allow actions on the Namespace object named "foo".
	// We check the Kind. Note: Unstructured objects (used by this controller) always have GVK populated.
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind == "Namespace" {
		// For a Namespace object, its Name IS the namespace identifier.
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
