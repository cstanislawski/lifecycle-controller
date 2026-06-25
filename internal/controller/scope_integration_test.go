package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Scoped Watch Integration", Serial, func() {

	const (
		AllowedNamespace = "integration-allowed"
		IgnoredNamespace = "integration-ignored"
	)

	BeforeEach(func() {
		ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: AllowedNamespace}}
		ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: IgnoredNamespace}}
		_ = k8sClient.Create(context.Background(), ns1)
		_ = k8sClient.Create(context.Background(), ns2)
	})

	AfterEach(func() {
		// Reset configuration to allow all
		Reconciler.Config = ScopeConfig{}
	})

	Context("When filtering by namespace", func() {
		It("should ignore events in ignored namespaces", func() {
			// Configure the controller to ignore the specific namespace
			Reconciler.Config = ScopeConfig{
				IgnoreNamespaces: []string{IgnoredNamespace},
			}

			ctx := context.Background()

			By("Creating a Deployment in the Ignored Namespace with immediate delete")
			ignoredDepName := "ignored-dep"
			ignoredDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ignoredDepName,
					Namespace: IgnoredNamespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation: "1s",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, ignoredDep)).To(Succeed())

			By("Creating a Deployment in the Allowed Namespace with immediate delete")
			allowedDepName := "allowed-dep"
			allowedDep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      allowedDepName,
					Namespace: AllowedNamespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation: "1s",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, allowedDep)).To(Succeed())

			By("Verifying the Allowed deployment is processed and deleted")
			Eventually(func(g Gomega) {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: allowedDepName, Namespace: AllowedNamespace}, &appsv1.Deployment{})
				g.Expect(err).To(HaveOccurred())
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed())

			By("Verifying the Ignored deployment is NOT processed and still exists")
			Consistently(func(g Gomega) {
				dep := &appsv1.Deployment{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: ignoredDepName, Namespace: IgnoredNamespace}, dep)
				g.Expect(err).NotTo(HaveOccurred())
				// Also verify it wasn't mutated (annotation should still be delete-after, not converted to delete-at)
				g.Expect(dep.Annotations).To(HaveKey(DeleteAfterAnnotation))
				g.Expect(dep.Annotations).NotTo(HaveKey(DeleteAtAnnotation))
			}, 2*time.Second, 500*time.Millisecond).Should(Succeed())

			_ = k8sClient.Delete(ctx, ignoredDep)
		})

		It("should allow watching the Namespace object itself when its name matches the watch list", func() {
			// This verifies the user's request: "the actual namespace(s, if they provide a pattern) provided"
			nsToWatch := "scope-test-ns-obj"
			Reconciler.Config = ScopeConfig{
				WatchNamespaces: []string{nsToWatch},
			}
			ctx := context.Background()

			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsToWatch,
					Annotations: map[string]string{
						DeleteAtAnnotation: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("Verifying the Namespace object itself is deleted (or terminating)")
			Eventually(func(g Gomega) {
				fetchedNS := &corev1.Namespace{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: nsToWatch}, fetchedNS)
				if err != nil {
					// If error occurred, it MUST be NotFound
					g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
				} else {
					// If found, it MUST be marked for deletion
					g.Expect(fetchedNS.DeletionTimestamp).NotTo(BeNil(), "Namespace should be in terminating state")
				}
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed())
		})
	})
})
