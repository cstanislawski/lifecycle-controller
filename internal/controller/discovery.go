package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const discoveryRetryInterval = 30 * time.Second

// Every watched resource may carry either deletion or restart annotations.
var requiredWatchVerbs = sets.New("get", "list", "watch", "patch", "delete")

type preferredResourceDiscovery interface {
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
}

type discoveredResource struct {
	key string
	gvk schema.GroupVersionKind
}

type resourcePlan struct {
	resources []discoveredResource
	counts    map[[2]string]int
}

type coverageState struct {
	mu     sync.RWMutex
	ready  bool
	reason string
}

func newCoverageState() *coverageState {
	discoveryReady.Set(0)
	return &coverageState{reason: "resource discovery has not completed"}
}

func (s *coverageState) markDegraded(reason string) {
	s.mu.Lock()
	s.ready = false
	s.reason = reason
	s.mu.Unlock()
	discoveryReady.Set(0)
}

func (s *coverageState) markReady() {
	s.mu.Lock()
	s.ready = true
	s.reason = ""
	s.mu.Unlock()
	discoveryReady.Set(1)
}

func (s *coverageState) check() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ready {
		return nil
	}
	return fmt.Errorf("resource coverage not ready: %s", s.reason)
}

// ReadinessCheck gates active instances on complete discovery and synchronized
// watch caches. A replica waiting for leader election has no active coverage
// responsibility and remains ready to take over.
// Manager.Elected is process-local and stays open on replicas that lose election.
func (r *LifecycleReconciler) ReadinessCheck(elected <-chan struct{}, leaderElectionEnabled bool) healthz.Checker {
	return func(_ *http.Request) error {
		if leaderElectionEnabled {
			select {
			case <-elected:
			default:
				return nil
			}
		}
		if r.coverage == nil {
			return errors.New("resource coverage not initialized")
		}
		return r.coverage.check()
	}
}

type controllerBuilder func([]discoveredResource) (func(context.Context) error, error)

type discoveryCoordinator struct {
	client        preferredResourceDiscovery
	config        ScopeConfig
	state         *coverageState
	logger        logr.Logger
	retryInterval time.Duration
	build         controllerBuilder
}

func (c *discoveryCoordinator) prepareInitial() (syncWait func(context.Context) error, retry bool, err error) {
	plan, err := c.discover()
	if err != nil {
		c.state.markDegraded(err.Error())
		if len(c.config.WatchResources) > 0 {
			return nil, false, fmt.Errorf("explicit resource discovery failed: %w", err)
		}
		c.logger.Info("Deferring controller setup until complete discovery succeeds", "retryInterval", c.retryInterval)
		return nil, true, nil
	}

	syncWait, err = c.build(plan.resources)
	if err != nil {
		c.state.markDegraded(err.Error())
		return nil, false, fmt.Errorf("failed to configure resource watches: %w", err)
	}
	c.state.markDegraded("waiting for resource watch caches to synchronize")
	return syncWait, false, nil
}

func (c *discoveryCoordinator) discover() (*resourcePlan, error) {
	lists, err := c.client.ServerPreferredResources()
	if err != nil {
		c.observeDiscoveryError(lists, err)
		return nil, fmt.Errorf("server preferred resources are incomplete: %w", err)
	}
	if len(lists) == 0 {
		err := errors.New("server returned no preferred resources")
		c.observeDiscoveryError(nil, err)
		return nil, err
	}

	plan, err := planResources(lists, c.config, c.logger)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (c *discoveryCoordinator) observeDiscoveryError(lists []*metav1.APIResourceList, err error) {
	resetDiscoveryResourceMetrics()
	failed := 1
	var groupErr *discovery.ErrGroupDiscoveryFailed
	if errors.As(err, &groupErr) {
		failed = len(groupErr.Groups)
		for gv, groupFailure := range groupErr.Groups {
			c.logger.Error(groupFailure, "Resource discovery failed for API group", "groupVersion", gv.String())
		}
	} else {
		c.logger.Error(err, "Resource discovery failed")
	}
	setDiscoveryResourceMetric(discoveryResultFailed, discoveryReasonGroupDiscovery, failed)
	if len(lists) > 0 {
		c.logger.Info("Discarding partial discovery results; no partial watch set will be started", "resourceLists", len(lists))
	}
}

func (c *discoveryCoordinator) Start(ctx context.Context) error {
	err := wait.PollUntilContextCancel(ctx, c.retryInterval, true, func(ctx context.Context) (bool, error) {
		plan, err := c.discover()
		if err != nil {
			c.state.markDegraded(err.Error())
			c.logger.Info("Complete resource discovery still unavailable; controller remains degraded")
			return false, nil
		}

		syncWait, err := c.build(plan.resources)
		if err != nil {
			c.state.markDegraded(err.Error())
			return false, fmt.Errorf("failed to configure resource watches after discovery recovery: %w", err)
		}
		c.state.markDegraded("waiting for resource watch caches to synchronize")
		if err := syncWait(ctx); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			c.state.markDegraded(err.Error())
			return false, fmt.Errorf("failed to synchronize resource watch caches after discovery recovery: %w", err)
		}
		c.state.markReady()
		c.logger.Info("Resource discovery recovered and all watch caches synchronized", "resources", len(plan.resources))
		return true, nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	<-ctx.Done()
	return nil
}

func (*discoveryCoordinator) NeedLeaderElection() bool {
	return true
}

type coverageSyncRunnable struct {
	state     *coverageState
	wait      func(context.Context) error
	logger    logr.Logger
	resources int
}

func (r *coverageSyncRunnable) Start(ctx context.Context) error {
	if err := r.wait(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		r.state.markDegraded(err.Error())
		return fmt.Errorf("failed to synchronize resource watch caches: %w", err)
	}
	r.state.markReady()
	r.logger.Info("All resource watch caches synchronized", "resources", r.resources)
	<-ctx.Done()
	return nil
}

func (*coverageSyncRunnable) NeedLeaderElection() bool {
	return true
}

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
	for _, pattern := range config.WatchResources {
		if !resolvedPatterns[pattern] {
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

type trackedSource struct {
	name   string
	source source.TypedSyncingSource[resourceRequest]
	done   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
	err    error
}

func newTrackedSource(name string, src source.TypedSyncingSource[resourceRequest]) *trackedSource {
	return &trackedSource{name: name, source: src, done: make(chan struct{})}
}

func (s *trackedSource) String() string {
	return "resource watch " + s.name
}

func (s *trackedSource) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[resourceRequest]) error {
	err := s.source.Start(ctx, queue)
	if err != nil {
		s.finish(err)
	}
	return err
}

func (s *trackedSource) WaitForSync(ctx context.Context) error {
	err := s.source.WaitForSync(ctx)
	s.finish(err)
	return err
}

func (s *trackedSource) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *trackedSource) wait(ctx context.Context) error {
	select {
	case <-s.done:
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.err != nil {
			setDiscoveryResourceMetric(discoveryResultFailed, discoveryReasonCacheSync, 1)
			return fmt.Errorf("resource %s: %w", s.name, s.err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *LifecycleReconciler) buildController(mgr ctrl.Manager, resources []discoveredResource) (func(context.Context) error, error) {
	if len(resources) == 0 {
		mgr.GetLogger().WithName("lifecycle-setup").Info("No resource watches requested after applying configuration")
		return func(context.Context) error { return nil }, nil
	}

	controllerLogger := mgr.GetLogger().WithValues("controller", "lifecycle")
	controllerBuilder := builder.TypedControllerManagedBy[resourceRequest](mgr).
		Named("lifecycle").
		WithLogConstructor(func(req *resourceRequest) logr.Logger {
			if req == nil {
				return controllerLogger
			}
			return controllerLogger.WithValues(
				"namespace", req.Namespace,
				"name", req.Name,
				"gvk", req.GVK.String(),
			)
		})
	lifecyclePredicate := LifecyclePredicate()
	namespacePredicate := NamespaceScopePredicate(func() ScopeConfig { return r.Config })
	setupLog := mgr.GetLogger().WithName("lifecycle-setup")
	tracked := make([]*trackedSource, 0, len(resources))

	for _, resource := range resources {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(resource.gvk)
		var watchedObject client.Object = object
		enqueue := handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []resourceRequest {
			return []resourceRequest{{NamespacedName: client.ObjectKeyFromObject(object), GVK: resource.gvk}}
		})
		src := source.TypedKind(mgr.GetCache(), watchedObject, enqueue, lifecyclePredicate, namespacePredicate)
		watch := newTrackedSource(resource.key, src)
		tracked = append(tracked, watch)
		controllerBuilder = controllerBuilder.WatchesRawSource(watch)
		setupLog.Info("Configuring resource watch", "resource", resource.key, "gvk", resource.gvk.String())
	}

	if _, err := controllerBuilder.Build(r); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		for _, watch := range tracked {
			if err := watch.wait(ctx); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

func (r *LifecycleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("lifecycle-controller")
	r.coverage = newCoverageState()

	discoveryClient := r.discovery
	if discoveryClient == nil {
		var err error
		discoveryClient, err = discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
		if err != nil {
			r.coverage.markDegraded(err.Error())
			return fmt.Errorf("failed to create discovery client: %w", err)
		}
	}

	configuredResources := 0
	coordinator := &discoveryCoordinator{
		client:        discoveryClient,
		config:        r.Config,
		state:         r.coverage,
		logger:        mgr.GetLogger().WithName("lifecycle-discovery"),
		retryInterval: discoveryRetryInterval,
		build: func(resources []discoveredResource) (func(context.Context) error, error) {
			configuredResources = len(resources)
			return r.buildController(mgr, resources)
		},
	}

	syncWait, retry, err := coordinator.prepareInitial()
	if err != nil {
		return err
	}
	if retry {
		return mgr.Add(coordinator)
	}
	return mgr.Add(&coverageSyncRunnable{
		state:     r.coverage,
		wait:      syncWait,
		logger:    coordinator.logger,
		resources: configuredResources,
	})
}

var _ source.TypedSyncingSource[resourceRequest] = (*trackedSource)(nil)
