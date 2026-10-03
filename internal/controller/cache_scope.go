package controller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// CacheOptions restricts API reads when the watch list contains exact namespace names.
func (c ScopeConfig) CacheOptions() (cache.Options, error) {
	if len(c.WatchNamespaces) == 0 {
		return cache.Options{}, nil
	}
	for _, namespace := range c.WatchNamespaces {
		if strings.ContainsAny(namespace, "*?[]\\") {
			return cache.Options{}, nil
		}
	}
	for _, namespace := range c.WatchNamespaces {
		if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
			return cache.Options{}, fmt.Errorf("invalid namespace name %q", namespace)
		}
	}
	options := cache.Options{DefaultNamespaces: make(map[string]cache.Config)}
	for _, namespace := range c.WatchNamespaces {
		if c.IsNamespaceAllowed(namespace) {
			options.DefaultNamespaces[namespace] = cache.Config{}
		}
	}
	if len(options.DefaultNamespaces) == 0 {
		return cache.Options{}, fmt.Errorf("watch list requires at least one namespace after ignore rules")
	}
	return options, nil
}
