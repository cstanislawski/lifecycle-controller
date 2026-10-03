package controller

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		cached   []string
		cacheErr string
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
		{
			name:   "Exact lists remove exclusions and duplicates",
			config: ScopeConfig{WatchNamespaces: []string{"team-a", "team-b", "team-a"}, IgnoreNamespaces: []string{"team-b"}},
			ns:     "team-a", expected: true, cached: []string{"team-a"},
		},
		{
			name:   "Two exact namespaces",
			config: ScopeConfig{WatchNamespaces: []string{"team-b", "team-a"}},
			ns:     "team-a", expected: true, cached: []string{"team-a", "team-b"},
		},
		{
			name:   "Mixed exact names and patterns require cluster reads",
			config: ScopeConfig{WatchNamespaces: []string{"team-a", "dev-?"}},
			ns:     "team-a", expected: true,
		},
		{
			name:   "Ignore glob removes one exact namespace",
			config: ScopeConfig{WatchNamespaces: []string{"team-a", "dev-b"}, IgnoreNamespaces: []string{"dev-*"}},
			ns:     "dev-b", cached: []string{"team-a"},
		},
		{
			name:   "All exact namespaces ignored",
			config: ScopeConfig{WatchNamespaces: []string{"team-a"}, IgnoreNamespaces: []string{"team-a"}},
			ns:     "team-a", cacheErr: "at least one namespace",
		},
		{
			name:   "Ignore glob excludes the entire exact list",
			config: ScopeConfig{WatchNamespaces: []string{"team-a"}, IgnoreNamespaces: []string{"team-*"}},
			ns:     "team-a", cacheErr: "at least one namespace",
		},
		{
			name:   "Invalid namespace name",
			config: ScopeConfig{WatchNamespaces: []string{"INVALID"}},
			ns:     "team-a", cacheErr: "invalid namespace name",
		},
		{
			name:     "Empty namespace name",
			config:   ScopeConfig{WatchNamespaces: []string{""}},
			expected: true, cacheErr: "invalid namespace name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.config.IsNamespaceAllowed(tt.ns)
			g.Expect(allowed).To(Equal(tt.expected))
			options, err := tt.config.CacheOptions()
			if tt.cacheErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.cacheErr) {
					t.Fatalf("want cache error containing %q, got %v", tt.cacheErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var namespaces []string
			for namespace := range options.DefaultNamespaces {
				namespaces = append(namespaces, namespace)
			}
			sort.Strings(namespaces)
			if !reflect.DeepEqual(namespaces, tt.cached) {
				t.Fatalf("cache namespaces: got %v, want %v", namespaces, tt.cached)
			}
		})
	}
}

func TestAllowLogic(t *testing.T) {
	g := NewGomegaWithT(t)

	// Helper to create objects
	mkObj := func(kind, name, namespace string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetKind(kind)
		u.SetAPIVersion("v1")
		u.SetName(name)
		u.SetNamespace(namespace)
		return u
	}

	tests := []struct {
		name     string
		config   ScopeConfig
		obj      client.Object
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
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"namespaces"}},
			obj:      mkObj("Namespace", "foo", ""),
			expected: true,
		},
		{
			name:     "Namespace Object Denied (Mismatch Watch)",
			config:   ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"namespaces"}},
			obj:      mkObj("Namespace", "bar", ""),
			expected: false,
		},
		{
			name:   "Namespace filter alone does not permit Namespace actions",
			config: ScopeConfig{WatchNamespaces: []string{"foo"}},
			obj:    mkObj("Namespace", "foo", ""),
		},
		{
			name:   "Resource wildcard does not permit Namespace actions with namespace filters",
			config: ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"*"}},
			obj:    mkObj("Namespace", "foo", ""),
		},
		{
			name:   "Ignore rule excludes explicitly selected Namespace objects",
			config: ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"namespaces"}, IgnoreResources: []string{"namespace*"}},
			obj:    mkObj("Namespace", "foo", ""),
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
		{
			name:   "Native Namespace without TypeMeta",
			config: ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"namespaces"}},
			obj:    &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}, expected: true,
		},
		{
			name:   "Custom Namespace kind is not a native Namespace",
			config: ScopeConfig{WatchNamespaces: []string{"foo"}, WatchResources: []string{"namespaces"}},
			obj: &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "example.com/v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "foo"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := allow(tt.config, tt.obj)
			g.Expect(allowed).To(Equal(tt.expected))
		})
	}
}
