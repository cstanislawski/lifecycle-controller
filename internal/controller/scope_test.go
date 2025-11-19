package controller

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIsResourceAllowed(t *testing.T) {
	g := NewGomegaWithT(t)

	tests := []struct {
		name     string
		config   ScopeConfig
		res      string
		group    string
		expected bool
	}{
		{
			name:     "Default Allow All",
			config:   ScopeConfig{},
			res:      "pods",
			group:    "",
			expected: true,
		},
		{
			name:     "Allow Specific Exact Match",
			config:   ScopeConfig{WatchResources: []string{"deployments.apps"}},
			res:      "deployments",
			group:    "apps",
			expected: true,
		},
		{
			name:     "Allow Specific Mismatch",
			config:   ScopeConfig{WatchResources: []string{"deployments.apps"}},
			res:      "pods",
			group:    "",
			expected: false,
		},
		{
			name:     "Allow Wildcard Group",
			config:   ScopeConfig{WatchResources: []string{"*.apps"}},
			res:      "statefulsets",
			group:    "apps",
			expected: true,
		},
		{
			name:     "Ignore Specific",
			config:   ScopeConfig{IgnoreResources: []string{"secrets"}},
			res:      "secrets",
			group:    "",
			expected: false,
		},
		{
			name: "Ignore Precedence over Allow",
			config: ScopeConfig{
				WatchResources:  []string{"*"},
				IgnoreResources: []string{"secrets"},
			},
			res:      "secrets",
			group:    "",
			expected: false,
		},
		{
			name:     "Complex Glob",
			config:   ScopeConfig{WatchResources: []string{"cron-*"}},
			res:      "cron-jobs",
			group:    "",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.config.IsResourceAllowed(tt.res, tt.group)
			g.Expect(allowed).To(Equal(tt.expected))
		})
	}
}

func TestIsNamespaceAllowed(t *testing.T) {
	g := NewGomegaWithT(t)

	tests := []struct {
		name     string
		config   ScopeConfig
		ns       string
		expected bool
	}{
		{
			name:     "Default Allow",
			config:   ScopeConfig{},
			ns:       "default",
			expected: true,
		},
		{
			name:     "Watch Glob",
			config:   ScopeConfig{WatchNamespaces: []string{"dev-*"}},
			ns:       "dev-team-1",
			expected: true,
		},
		{
			name:     "Watch Glob Mismatch",
			config:   ScopeConfig{WatchNamespaces: []string{"dev-*"}},
			ns:       "prod",
			expected: false,
		},
		{
			name:     "Ignore Glob",
			config:   ScopeConfig{IgnoreNamespaces: []string{"kube-*"}},
			ns:       "kube-system",
			expected: false,
		},
		{
			name:     "Ignore Specific",
			config:   ScopeConfig{IgnoreNamespaces: []string{"default"}},
			ns:       "default",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.config.IsNamespaceAllowed(tt.ns)
			g.Expect(allowed).To(Equal(tt.expected))
		})
	}
}

func TestAllowLogic(t *testing.T) {
	g := NewGomegaWithT(t)

	// Helper to create objects
	mkObj := func(kind, name, namespace string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetKind(kind)
		u.SetName(name)
		u.SetNamespace(namespace)
		return u
	}

	tests := []struct {
		name     string
		config   ScopeConfig
		obj      *unstructured.Unstructured
		expected bool
	}{
		{
			name:     "Standard Namespaced Allow",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      mkObj("Pod", "p1", "foo"),
			expected: true,
		},
		{
			name:     "Standard Namespaced Deny",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      mkObj("Pod", "p1", "bar"),
			expected: false,
		},
		{
			name:     "Namespace Object Allowed (Matches Watch)",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      mkObj("Namespace", "foo", ""),
			expected: true,
		},
		{
			name:     "Namespace Object Denied (Mismatch Watch)",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      mkObj("Namespace", "bar", ""),
			expected: false,
		},
		{
			name:     "Cluster Resource Denied (Strict Mode)",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      mkObj("Node", "node-1", ""),
			expected: false, // Because we restricted to 'foo', global nodes are out
		},
		{
			name:     "Cluster Resource Allowed (Default Mode)",
			config:   ScopeConfig{},
			obj:      mkObj("Node", "node-1", ""),
			expected: true,
		},
		{
			name:     "Cluster Resource Allowed (Ignore Mode only)",
			config:   ScopeConfig{IgnoreNamespaces: []string{"kube-system"}},
			obj:      mkObj("Node", "node-1", ""),
			expected: true, // Explicit ignore list doesn't trigger strict "only watch X" mode
		},
		{
			name:     "Typed Object Namespace Check",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:      &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"namespace": "foo"}}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := allow(tt.config, tt.obj)
			g.Expect(allowed).To(Equal(tt.expected))
		})
	}
}
