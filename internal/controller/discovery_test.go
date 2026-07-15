package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

var watchVerbs = metav1.Verbs{"get", "list", "watch", "patch", "delete"}

type discoveryResponse struct {
	lists []*metav1.APIResourceList
	err   error
}

type scriptedDiscovery struct {
	mu        sync.Mutex
	responses []discoveryResponse
	calls     int
}

func (d *scriptedDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	response := d.responses[len(d.responses)-1]
	if d.calls < len(d.responses) {
		response = d.responses[d.calls]
	}
	d.calls++
	return response.lists, response.err
}

func (d *scriptedDiscovery) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func coreResources(resources ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: "v1", APIResources: resources}
}

func apiResource(name, kind string, verbs metav1.Verbs) metav1.APIResource {
	return metav1.APIResource{Name: name, Kind: kind, Namespaced: true, Verbs: verbs}
}

func newTestCoordinator(client preferredResourceDiscovery, config ScopeConfig, build controllerBuilder) *discoveryCoordinator {
	return &discoveryCoordinator{
		client:        client,
		config:        config,
		state:         newCoverageState(),
		logger:        logr.Discard(),
		retryInterval: time.Millisecond,
		build:         build,
	}
}

func TestExplicitDiscoverySetupRejectsDiscoveryErrors(t *testing.T) {
	partialErr := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("temporarily unavailable"),
	}}
	tests := map[string]discoveryResponse{
		"total error": {
			err: errors.New("API server unavailable"),
		},
		"partial error": {
			lists: []*metav1.APIResourceList{coreResources(apiResource("pods", "Pod", watchVerbs))},
			err:   partialErr,
		},
	}

	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			built := false
			coordinator := newTestCoordinator(
				&scriptedDiscovery{responses: []discoveryResponse{response}},
				ScopeConfig{WatchResources: []string{"pods"}},
				func([]discoveredResource) (func(context.Context) error, error) {
					built = true
					return func(context.Context) error { return nil }, nil
				},
			)

			_, retry, err := coordinator.prepareInitial()
			if err == nil || !strings.Contains(err.Error(), "explicit resource discovery failed") {
				t.Fatalf("expected explicit discovery setup failure, got %v", err)
			}
			if retry {
				t.Fatal("explicit allowlists must not enter broad-mode retry")
			}
			if built {
				t.Fatal("controller must not be built from incomplete discovery")
			}
			if err := coordinator.state.check(); err == nil {
				t.Fatal("readiness must remain degraded after discovery failure")
			}
		})
	}
}

func TestExplicitDiscoverySetupRejectsMissingAndUnwatchableResources(t *testing.T) {
	tests := map[string]struct {
		config   ScopeConfig
		response discoveryResponse
		want     string
	}{
		"missing requested resource": {
			config: ScopeConfig{WatchResources: []string{"deployments.apps"}},
			response: discoveryResponse{lists: []*metav1.APIResourceList{
				coreResources(apiResource("pods", "Pod", watchVerbs)),
			}},
			want: "missing patterns=[deployments.apps]",
		},
		"requested resource lacks watch verbs": {
			config: ScopeConfig{WatchResources: []string{"pods"}},
			response: discoveryResponse{lists: []*metav1.APIResourceList{
				coreResources(apiResource("pods", "Pod", metav1.Verbs{"get", "list"})),
			}},
			want: "unwatchable resources=[pods]",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			built := false
			coordinator := newTestCoordinator(
				&scriptedDiscovery{responses: []discoveryResponse{test.response}},
				test.config,
				func([]discoveredResource) (func(context.Context) error, error) {
					built = true
					return func(context.Context) error { return nil }, nil
				},
			)

			_, retry, err := coordinator.prepareInitial()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected error containing %q, got %v", test.want, err)
			}
			if retry || built {
				t.Fatalf("invalid explicit setup: retry=%v built=%v", retry, built)
			}
		})
	}
}

func TestBroadDiscoveryRecoversBeforeBecomingReady(t *testing.T) {
	partialErr := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "apps", Version: "v1"}: errors.New("temporarily unavailable"),
	}}
	client := &scriptedDiscovery{responses: []discoveryResponse{
		{
			lists: []*metav1.APIResourceList{coreResources(apiResource("pods", "Pod", watchVerbs))},
			err:   partialErr,
		},
		{err: errors.New("API server unavailable")},
		{lists: []*metav1.APIResourceList{coreResources(apiResource("pods", "Pod", watchVerbs))}},
	}}
	syncStarted := make(chan struct{})
	releaseSync := make(chan struct{})
	coordinator := newTestCoordinator(client, ScopeConfig{}, func(resources []discoveredResource) (func(context.Context) error, error) {
		if len(resources) != 1 || resources[0].key != "pods" {
			return nil, fmt.Errorf("unexpected recovered resources: %#v", resources)
		}
		return func(ctx context.Context) error {
			close(syncStarted)
			select {
			case <-releaseSync:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, nil
	})

	_, retry, err := coordinator.prepareInitial()
	if err != nil || !retry {
		t.Fatalf("broad partial discovery should defer and retry: retry=%v err=%v", retry, err)
	}
	if err := coordinator.state.check(); err == nil {
		t.Fatal("partial discovery must be unready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coordinator.Start(ctx) }()

	select {
	case <-syncStarted:
	case <-time.After(time.Second):
		t.Fatal("discovery did not recover")
	}
	if err := coordinator.state.check(); err == nil {
		t.Fatal("complete discovery without cache sync must remain unready")
	}
	close(releaseSync)
	eventually(t, time.Second, func() bool { return coordinator.state.check() == nil })
	if client.callCount() < 3 {
		t.Fatalf("expected retries through total error to recovery, got %d calls", client.callCount())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("coordinator shutdown failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop")
	}
}

func TestCacheSyncFailureKeepsCoverageUnready(t *testing.T) {
	state := newCoverageState()
	runnable := &coverageSyncRunnable{
		state:  state,
		wait:   func(context.Context) error { return errors.New("pods cache did not sync") },
		logger: logr.Discard(),
	}

	err := runnable.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pods cache did not sync") {
		t.Fatalf("expected cache synchronization failure, got %v", err)
	}
	if err := state.check(); err == nil {
		t.Fatal("cache synchronization failure must remain unready")
	}
}

func TestReadinessCheckTreatsOnlyPassiveLeaderReplicaAsReady(t *testing.T) {
	state := newCoverageState()
	reconciler := &LifecycleReconciler{coverage: state}
	elected := make(chan struct{})
	request, err := http.NewRequest(http.MethodGet, "/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.ReadinessCheck(elected, true)(request); err != nil {
		t.Fatalf("passive replica should be ready: %v", err)
	}
	close(elected)
	if err := reconciler.ReadinessCheck(elected, true)(request); err == nil {
		t.Fatal("elected replica must report degraded coverage")
	}
	if err := reconciler.ReadinessCheck(nil, false)(request); err == nil {
		t.Fatal("non-leader-election instance must report degraded coverage")
	}

	state.markReady()
	if err := reconciler.ReadinessCheck(elected, true)(request); err != nil {
		t.Fatalf("synchronized elected replica should be ready: %v", err)
	}
}

func TestExplicitIgnorePrecedenceCanProduceEmptyWatchSet(t *testing.T) {
	for name, lists := range map[string][]*metav1.APIResourceList{
		"resource present": {coreResources(apiResource("configmaps", "ConfigMap", watchVerbs))},
		"resource absent":  {coreResources(apiResource("pods", "Pod", watchVerbs))},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := planResources(
				lists,
				ScopeConfig{WatchResources: []string{"configmaps"}, IgnoreResources: []string{"configmaps"}},
				logr.Discard(),
			)
			if err != nil {
				t.Fatalf("ignore precedence should be valid: %v", err)
			}
			if len(plan.resources) != 0 {
				t.Fatalf("expected no watches, got %#v", plan.resources)
			}
		})
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
