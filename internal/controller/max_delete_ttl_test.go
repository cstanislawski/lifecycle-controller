package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Max delete TTL", func() {
	const (
		TestNamespace = "default"
		Timeout       = time.Second * 10
		Interval      = time.Millisecond * 250
	)

	BeforeEach(func() {
		Reconciler.MaxDeleteTTL = 30 * 24 * time.Hour
	})

	AfterEach(func() {
		Reconciler.MaxDeleteTTL = 0
	})

	newDeployment := func(key types.NamespacedName, annotations map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:        key.Name,
				Namespace:   key.Namespace,
				Annotations: annotations,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": key.Name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": key.Name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "workload", Image: "nginx"}}},
				},
			},
		}
	}

	expectDeleteTTLExceededEvent := func(ctx context.Context, key types.NamespacedName) {
		Eventually(func(g Gomega) {
			eventList := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, eventList, client.InNamespace(key.Namespace))).To(Succeed())

			found := false
			for _, event := range eventList.Items {
				if event.InvolvedObject.Name == key.Name && event.Reason == "DeleteTTLExceeded" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue())
		}, Timeout, Interval).Should(Succeed())
	}

	It("should ignore delete-after when it exceeds the configured maximum TTL", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "max-ttl-delete-after-denied", Namespace: TestNamespace}
		deployment := newDeployment(key, map[string]string{DeleteAfterAnnotation: "31d"})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		Consistently(func(g Gomega) {
			fetched := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
			g.Expect(fetched.Annotations).To(HaveKeyWithValue(DeleteAfterAnnotation, "31d"))
			g.Expect(fetched.Annotations).NotTo(HaveKey(DeleteAtAnnotation))
		}, 3*time.Second, Interval).Should(Succeed())
		expectDeleteTTLExceededEvent(ctx, key)

		Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
	})

	It("should convert delete-after when it is within the configured maximum TTL", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "max-ttl-delete-after-allowed", Namespace: TestNamespace}
		deployment := newDeployment(key, map[string]string{DeleteAfterAnnotation: "30d"})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		Eventually(func(g Gomega) {
			fetched := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
			g.Expect(fetched.Annotations).To(HaveKey(DeleteAtAnnotation))
			g.Expect(fetched.Annotations).NotTo(HaveKey(DeleteAfterAnnotation))
		}, Timeout, Interval).Should(Succeed())

		Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
	})

	It("should ignore future delete-at when it exceeds the configured maximum TTL", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: "max-ttl-delete-at-denied", Namespace: TestNamespace}
		deleteAt := time.Now().UTC().Add(31 * 24 * time.Hour).Format(time.RFC3339)
		deployment := newDeployment(key, map[string]string{DeleteAtAnnotation: deleteAt})
		Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

		Consistently(func(g Gomega) {
			fetched := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
			g.Expect(fetched.Annotations).To(HaveKeyWithValue(DeleteAtAnnotation, deleteAt))
		}, 3*time.Second, Interval).Should(Succeed())
		expectDeleteTTLExceededEvent(ctx, key)

		Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
	})
})
