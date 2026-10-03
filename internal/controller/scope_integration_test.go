package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimeconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var _ = Describe("Scoped Watch Integration", Serial, func() {
	BeforeEach(func() {
		// Only the restricted manager can process objects in these scenarios.
		previous := Reconciler.Config
		Reconciler.Config = ScopeConfig{WatchNamespaces: []string{"inactive-scope-controller"}}
		DeferCleanup(func() { Reconciler.Config = previous })
	})

	Context("When filtering by namespace", func() {
		It("should ignore events in ignored namespaces", func() {
			const userName = "namespace-scoped-controller"
			allowed := []string{"integration-allowed", "integration-allowed-b"}
			const ignored = "integration-ignored"
			namespaces := append(append([]string{}, allowed...), ignored)
			limited, userConfig := scopeUser(userName)
			for _, namespace := range namespaces {
				createScopeObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
				if namespace == ignored {
					continue
				}
				role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "scope-controller", Namespace: namespace},
					Rules: []rbacv1.PolicyRule{
						{APIGroups: []string{"apps"}, Resources: []string{"deployments"},
							Verbs: []string{"get", "list", "watch", "patch", "update", "delete"}},
						{APIGroups: []string{"", "events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
					},
				}
				binding := &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: namespace},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
					Subjects:   scopeSubjects(userName),
				}
				createScopeObjects(role, binding)
			}
			for _, namespace := range allowed {
				Eventually(func() error {
					return limited.List(ctx, &appsv1.DeploymentList{}, client.InNamespace(namespace))
				}, 10*time.Second).Should(Succeed())
			}
			By("checking denied API reads before controller startup")
			for _, namespace := range []string{"", ignored} {
				Expect(apierrors.IsForbidden(limited.List(ctx, &appsv1.DeploymentList{}, client.InNamespace(namespace)))).To(BeTrue())
			}
			Expect(apierrors.IsForbidden(limited.List(ctx, &corev1.SecretList{}, client.InNamespace(allowed[0])))).To(BeTrue())
			Expect(apierrors.IsForbidden(limited.List(ctx, &corev1.NamespaceList{}))).To(BeTrue())
			manager := startScopeManager(userConfig, ScopeConfig{
				WatchResources:  []string{"deployments.apps"},
				WatchNamespaces: namespaces, IgnoreNamespaces: []string{ignored},
			})

			By("creating annotated Deployments in both allowed namespaces and the ignored namespace")
			for _, namespace := range namespaces {
				createScopeObjects(&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name: "scheduled-delete", Namespace: namespace,
						Annotations: map[string]string{DeleteAfterAnnotation: "1s"},
					},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx"}}},
						},
					},
				})
			}
			for _, namespace := range allowed {
				Eventually(func() bool {
					return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{
						Name: "scheduled-delete", Namespace: namespace,
					}, &appsv1.Deployment{}))
				}, 10*time.Second, 500*time.Millisecond).Should(BeTrue())
			}
			By("verifying ignored objects remain unchanged for the existing observation window")
			key := client.ObjectKey{Name: "scheduled-delete", Namespace: ignored}
			Consistently(func(g Gomega) {
				object := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, object)).To(Succeed())
				g.Expect(object.Annotations).To(HaveKey(DeleteAfterAnnotation))
				g.Expect(object.Annotations).NotTo(HaveKey(DeleteAtAnnotation))
			}, 5*time.Second, 500*time.Millisecond).Should(Succeed())
			outside := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			Expect(apierrors.IsForbidden(limited.Delete(ctx, outside))).To(BeTrue())
			Expect(manager.GetClient().Get(ctx, key, &appsv1.Deployment{})).NotTo(Succeed())
		})

		It("should allow watching the Namespace object itself when its name matches the watch list", func() {
			const (
				allowed  = "scope-test-ns-obj"
				denied   = "scope-test-ns-denied"
				userName = "namespace-object-controller"
			)
			limited, userConfig := scopeUser(userName)
			for _, name := range []string{allowed, denied} {
				createScopeObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
			}
			// The API server provides default; this test does not own it.
			err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
			Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue())
			role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "scope-namespace-controller"},
				Rules: []rbacv1.PolicyRule{
					{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch"}},
					{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{allowed},
						Verbs: []string{"patch", "update", "delete"}},
				},
			}
			binding := &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: role.Name}, Subjects: scopeSubjects(userName),
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			}
			eventRole := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "scope-namespace-events", Namespace: "default"},
				Rules: []rbacv1.PolicyRule{{APIGroups: []string{"", "events.k8s.io"}, Resources: []string{"events"},
					Verbs: []string{"create", "patch"}}},
			}
			eventBinding := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: eventRole.Name, Namespace: "default"}, Subjects: scopeSubjects(userName),
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: eventRole.Name},
			}
			createScopeObjects(role, binding, eventRole, eventBinding)
			Eventually(func() error { return limited.List(ctx, &corev1.NamespaceList{}) }, 10*time.Second).Should(Succeed())
			startScopeManager(userConfig, ScopeConfig{WatchNamespaces: []string{allowed}, WatchResources: []string{"namespaces"}})

			for _, name := range []string{allowed, denied} {
				object := &corev1.Namespace{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, object)).To(Succeed())
				before := object.DeepCopy()
				object.Annotations = map[string]string{DeleteAfterAnnotation: "1s"}
				if name == denied {
					Expect(apierrors.IsForbidden(limited.Patch(ctx, object, client.MergeFrom(before)))).To(BeTrue())
					Expect(apierrors.IsForbidden(limited.Delete(ctx, object))).To(BeTrue())
				}
				Expect(k8sClient.Patch(ctx, object, client.MergeFrom(before))).To(Succeed())
			}
			Eventually(func() bool {
				object := &corev1.Namespace{}
				err := k8sClient.Get(ctx, client.ObjectKey{Name: allowed}, object)
				return apierrors.IsNotFound(err) || (err == nil && object.DeletionTimestamp != nil)
			}, 10*time.Second, 500*time.Millisecond).Should(BeTrue())
			outside := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: denied}, outside)).To(Succeed())
			Expect(outside.DeletionTimestamp).To(BeNil())
			Expect(outside.Annotations).NotTo(HaveKey(DeleteAtAnnotation))
			Eventually(func() bool {
				events := &eventsv1.EventList{}
				if err := k8sClient.List(ctx, events, client.InNamespace("default")); err != nil {
					return false
				}
				for _, event := range events.Items {
					if event.Regarding.Name == allowed && event.Reason == "Deleted" {
						return true
					}
				}
				return false
			}, 10*time.Second).Should(BeTrue())
		})
	})
})

func scopeUser(name string) (client.Client, *rest.Config) {
	GinkgoHelper()
	user, err := testEnv.AddUser(envtest.User{Name: name}, cfg)
	Expect(err).NotTo(HaveOccurred())
	limited, err := client.New(user.Config(), client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return limited, user.Config()
}

func scopeSubjects(name string) []rbacv1.Subject {
	return []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: name}}
}

func createScopeObjects(objects ...client.Object) {
	GinkgoHelper()
	for _, object := range objects {
		Expect(k8sClient.Create(ctx, object)).To(Succeed())
		DeferCleanup(func() {
			err := k8sClient.Delete(ctx, object)
			Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue())
		})
	}
}

func startScopeManager(config *rest.Config, scope ScopeConfig) ctrl.Manager {
	GinkgoHelper()
	options, err := scope.CacheOptions()
	Expect(err).NotTo(HaveOccurred())
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme.Scheme, Cache: options, Metrics: metricsserver.Options{BindAddress: "0"},
		// The suite manager already uses the lifecycle controller name.
		Controller: runtimeconfig.Controller{SkipNameValidation: ptr.To(true)},
	})
	Expect(err).NotTo(HaveOccurred())
	reconciler := &LifecycleReconciler{Client: manager.GetClient(), Scheme: manager.GetScheme(), Config: scope}
	Expect(reconciler.SetupWithManager(manager)).To(Succeed())
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- manager.Start(runCtx) }()
	DeferCleanup(func() {
		stop()
		Eventually(done, 10*time.Second).Should(Receive(BeNil()))
	})
	Eventually(func() error { return reconciler.coverage.check() }, 20*time.Second).Should(Succeed())
	return manager
}
