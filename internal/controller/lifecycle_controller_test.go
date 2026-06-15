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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Lifecycle Controller", func() {

	const (
		TestNamespace = "default"
		Timeout       = time.Second * 10
		Interval      = time.Millisecond * 250
	)

	expectEventReason := func(ctx context.Context, namespace, objectName, reason string) {
		Eventually(func(g Gomega) {
			eventList := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, eventList, client.InNamespace(namespace))).To(Succeed())
			found := false
			for _, event := range eventList.Items {
				if event.InvolvedObject.Name == objectName && event.Reason == reason {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue())
		}, Timeout, Interval).Should(Succeed())
	}

	Context("when handling deletion annotations", func() {
		It("should convert 'delete-after' to a 'delete-at' annotation", func() {
			By("Creating a new Deployment with a delete-after annotation")
			ctx := context.Background()
			deploymentName := "test-deployment-delete-after"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: TestNamespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation: "1h",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).Should(Succeed())

			// Eventually is used to wait for the asynchronous controller to act.
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: TestNamespace}, fetched)).To(Succeed())
				ann := fetched.GetAnnotations()
				g.Expect(ann).To(HaveKey(DeleteAtAnnotation))
				g.Expect(ann).ToNot(HaveKey(DeleteAfterAnnotation))
				g.Expect(ann).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))

				// Check if the timestamp is roughly correct
				deleteAtTime, err := time.Parse(time.RFC3339, ann[DeleteAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deleteAtTime).To(BeTemporally("~", time.Now().Add(time.Hour), time.Minute))
			}, Timeout, Interval).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should handle extended duration formats like 'd' for days", func() {
			By("Creating a new Deployment with '3d' delete-after annotation")
			ctx := context.Background()
			deploymentName := "test-deployment-delete-after-days"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: TestNamespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation: "3d",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).Should(Succeed())

			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: TestNamespace}, fetched)).To(Succeed())
				ann := fetched.GetAnnotations()
				g.Expect(ann).To(HaveKey(DeleteAtAnnotation), "The delete-at annotation should have been added")

				deleteAtTime, err := time.Parse(time.RFC3339, ann[DeleteAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
				// Check that the deletion time is approximately 72 hours from now
				g.Expect(deleteAtTime).To(BeTemporally("~", time.Now().Add(72*time.Hour), time.Minute))
			}, Timeout, Interval).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should delete a resource when 'delete-at' is in the past", func() {
			By("Creating a new Deployment with a past delete-at annotation")
			ctx := context.Background()
			deploymentName := "test-deployment-delete-at-past"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: TestNamespace,
					Annotations: map[string]string{
						DeleteAtAnnotation: time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).Should(Succeed())

			// Eventually, the deployment should be gone.
			Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), &appsv1.Deployment{})
				return apierrors.IsNotFound(err)
			}, Timeout, Interval).Should(BeTrue())
		})
	})

	Context("when handling restart annotations", func() {
		It("should convert 'restart-after' to a 'restart-at' annotation", func() {
			By("Creating a new Deployment with a restart-after annotation")
			ctx := context.Background()
			deploymentName := "test-deployment-restart-after"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: TestNamespace,
					Annotations: map[string]string{
						RestartAfterAnnotation: "30m",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).Should(Succeed())

			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: TestNamespace}, fetched)).To(Succeed())
				ann := fetched.GetAnnotations()
				g.Expect(ann).To(HaveKey(RestartAtAnnotation))
				g.Expect(ann).ToNot(HaveKey(RestartAfterAnnotation))
				g.Expect(ann).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))
				g.Expect(fetched.Spec.Template.Annotations).ToNot(HaveKey(ManagedByAnnotation))

				restartAtTime, err := time.Parse(time.RFC3339, ann[RestartAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(restartAtTime).To(BeTemporally("~", time.Now().Add(30*time.Minute), time.Minute))
			}, Timeout, Interval).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should trigger a restart and clean up 'restart-at' annotation", func() {
			By("Creating a new Deployment with a past restart-at annotation")
			ctx := context.Background()
			deploymentName := "test-deployment-restart-at"
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deploymentName,
					Namespace: TestNamespace,
					Annotations: map[string]string{
						RestartAtAnnotation: time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      map[string]string{"app": "test"},
							Annotations: map[string]string{}, // Ensure annotations map exists
						},
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).Should(Succeed())

			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(deployment), fetched)).To(Succeed())

				// Check that the main annotation is gone
				g.Expect(fetched.Annotations).ToNot(HaveKey(RestartAtAnnotation))
				g.Expect(fetched.Annotations).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))

				// Check that the template annotation was added to trigger the restart
				templateAnnotations := fetched.Spec.Template.Annotations
				g.Expect(templateAnnotations).To(HaveKey(RestartedAtTemplate))
				g.Expect(templateAnnotations).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))
			}, Timeout, Interval).Should(Succeed())

			// Cleanup
			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should trigger a recurring restart with 'restart-every'", func() {
			By("Creating a deployment with restart-every: 1s")
			ctx := context.Background()
			key := types.NamespacedName{Name: "restart-every-test", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{RestartEveryAnnotation: "1s"},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			By("Waiting for the controller to record recurring restart state")
			var previousLastRestart string
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKey(LastRestartTimestamp))
				g.Expect(fetched.Annotations).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))
				previousLastRestart = fetched.Annotations[LastRestartTimestamp]
			}, Timeout, Interval).Should(Succeed())

			By("Waiting for the next recurring restart to be triggered")
			var firstRestartTime string
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKey(LastRestartTimestamp))
				g.Expect(fetched.Annotations[LastRestartTimestamp]).NotTo(Equal(previousLastRestart))
				g.Expect(fetched.Spec.Template.Annotations).To(HaveKey(RestartedAtTemplate))
				g.Expect(fetched.Spec.Template.Annotations).To(HaveKeyWithValue(ManagedByAnnotation, ManagedByValue))
				firstRestartTime = fetched.Spec.Template.Annotations[RestartedAtTemplate]
				previousLastRestart = fetched.Annotations[LastRestartTimestamp]
			}, Timeout, Interval).Should(Succeed())

			By("Waiting for a second restart to confirm recurrence")
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKey(LastRestartTimestamp))
				g.Expect(fetched.Annotations[LastRestartTimestamp]).NotTo(Equal(previousLastRestart))
				g.Expect(fetched.Spec.Template.Annotations).To(HaveKey(RestartedAtTemplate))
				g.Expect(fetched.Spec.Template.Annotations[RestartedAtTemplate]).NotTo(Equal(firstRestartTime))
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("when handling re-application of annotations", func() {
		It("should reset the timer when 'delete-after' is re-applied", func() {
			By("Creating a deployment with delete-after")
			ctx := context.Background()
			key := types.NamespacedName{Name: "re-apply-delete", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{DeleteAfterAnnotation: "1h"},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			var firstDeleteAt time.Time
			By("Waiting for the initial conversion to delete-at")
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).ToNot(HaveKey(DeleteAfterAnnotation))
				g.Expect(fetched.Annotations).To(HaveKey(DeleteAtAnnotation))
				var err error
				firstDeleteAt, err = time.Parse(time.RFC3339, fetched.Annotations[DeleteAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
			}, Timeout, Interval).Should(Succeed())

			By("Re-applying the 'delete-after' annotation")
			sleepPastTimestampSecond()
			updated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Annotations[DeleteAfterAnnotation] = "1h"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			By("Verifying the delete-at timestamp is updated")
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).ToNot(HaveKey(DeleteAfterAnnotation))
				g.Expect(fetched.Annotations).To(HaveKey(DeleteAtAnnotation))
				newDeleteAt, err := time.Parse(time.RFC3339, fetched.Annotations[DeleteAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(newDeleteAt.After(firstDeleteAt)).To(BeTrue(), "new delete time should be after the original")
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should reset the timer when 'restart-after' is re-applied", func() {
			By("Creating a deployment with restart-after")
			ctx := context.Background()
			key := types.NamespacedName{Name: "re-apply-restart", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{RestartAfterAnnotation: "1h"},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			var firstRestartAt time.Time
			By("Waiting for the initial conversion to restart-at")
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).ToNot(HaveKey(RestartAfterAnnotation))
				g.Expect(fetched.Annotations).To(HaveKey(RestartAtAnnotation))
				var err error
				firstRestartAt, err = time.Parse(time.RFC3339, fetched.Annotations[RestartAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
			}, Timeout, Interval).Should(Succeed())

			By("Re-applying the 'restart-after' annotation")
			sleepPastTimestampSecond()
			updated := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Annotations[RestartAfterAnnotation] = "1h"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			By("Verifying the restart-at timestamp is updated")
			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).ToNot(HaveKey(RestartAfterAnnotation))
				g.Expect(fetched.Annotations).To(HaveKey(RestartAtAnnotation))
				newRestartAt, err := time.Parse(time.RFC3339, fetched.Annotations[RestartAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(newRestartAt.After(firstRestartAt)).To(BeTrue(), "new restart time should be after the original")
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})

	Context("when using reference-point annotation", func() {
		It("should use creationTimestamp for delete-after when specified", func() {
			By("Creating a deployment with reference-point: creationTimestamp")
			ctx := context.Background()
			key := types.NamespacedName{Name: "ref-point-delete", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation:    "1h",
						ReferencePointAnnotation: "creationTimestamp",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// We need the actual creation timestamp from the server
			fetchedOnCreate := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, fetchedOnCreate)).To(Succeed())
			creationTimestamp := fetchedOnCreate.GetCreationTimestamp().Time

			sleepPastTimestampSecond()

			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				ann := fetched.GetAnnotations()
				g.Expect(ann).To(HaveKey(DeleteAtAnnotation))

				deleteAtTime, err := time.Parse(time.RFC3339, ann[DeleteAtAnnotation])
				g.Expect(err).NotTo(HaveOccurred())

				// The calculated deletion time should be based on the creation time, not the reconciliation time.
				expectedDeleteAt := creationTimestamp.Add(time.Hour)
				g.Expect(deleteAtTime).To(BeTemporally("~", expectedDeleteAt, time.Second))
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should not act on conflicting annotations", func() {
			By("Creating a deployment with both delete and restart annotations")
			ctx := context.Background()
			key := types.NamespacedName{Name: "conflict-test", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						DeleteAfterAnnotation:  "1s",
						RestartAfterAnnotation: "1s",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			expectEventReason(ctx, TestNamespace, key.Name, "ConflictingAnnotations")

			fetched := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
			Expect(fetched.Annotations).To(HaveKey(DeleteAfterAnnotation))
			Expect(fetched.Annotations).To(HaveKey(RestartAfterAnnotation))
			Expect(fetched.Annotations).ToNot(HaveKey(DeleteAtAnnotation))
			Expect(fetched.Annotations).ToNot(HaveKey(RestartAtAnnotation))

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should not restart a non-pod-spawning resource like a ConfigMap", func() {
			By("Creating a ConfigMap with a restart annotation")
			ctx := context.Background()
			key := types.NamespacedName{Name: "cm-restart-test", Namespace: TestNamespace}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						RestartAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
					},
				},
				Data: map[string]string{"key": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			expectEventReason(ctx, TestNamespace, key.Name, "NotPodSpawner")

			fetched := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
			Expect(fetched.Annotations).To(HaveKey(RestartAtAnnotation))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("should not act and should record an event for an invalid cron-timezone", func() {
			By("Creating a deployment with restart-cron and invalid cron-timezone")
			ctx := context.Background()
			key := types.NamespacedName{Name: "invalid-cron-tz", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						RestartCronAnnotation:  "*/1 * * * *",
						CronTimezoneAnnotation: "Invalid/Timezone",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			// Wait for an event to be recorded
			Eventually(func(g Gomega) {
				eventList := &corev1.EventList{}
				g.Expect(k8sClient.List(ctx, eventList, client.InNamespace(TestNamespace))).To(Succeed())
				found := false
				for _, event := range eventList.Items {
					if event.InvolvedObject.Name == key.Name && event.Reason == "InvalidTimezone" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue())
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should ignore cron-timezone when restart-cron is not set", func() {
			By("Creating a deployment with restart-every and cron-timezone")
			ctx := context.Background()
			key := types.NamespacedName{Name: "ignored-cron-tz", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						RestartEveryAnnotation: "24h",
						CronTimezoneAnnotation: "Europe/Warsaw",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			Eventually(func(g Gomega) {
				fetched := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, key, fetched)).To(Succeed())
				g.Expect(fetched.Annotations).To(HaveKey(LastRestartTimestamp))
			}, Timeout, Interval).Should(Succeed())

			Eventually(func(g Gomega) {
				eventList := &corev1.EventList{}
				g.Expect(k8sClient.List(ctx, eventList, client.InNamespace(TestNamespace))).To(Succeed())
				found := false
				for _, event := range eventList.Items {
					if event.InvolvedObject.Name == key.Name && event.Reason == "IgnoredAnnotation" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue())
			}, Timeout, Interval).Should(Succeed())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should not delete a resource if dry-run is enabled", func() {
			By("Creating a deployment with delete-at in the past and dry-run")
			ctx := context.Background()
			key := types.NamespacedName{Name: "dry-run-delete", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						DeleteAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
						DryRunAnnotation:   "true",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			expectEventReason(ctx, TestNamespace, key.Name, "DryRunDelete")

			err := k8sClient.Get(ctx, key, &appsv1.Deployment{})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})

		It("should not delete a resource if global dry-run is enabled", func() {
			By("Enabling global dry-run and creating a deployment with delete-at in the past")
			Reconciler.GlobalDryRun = true
			DeferCleanup(func() { Reconciler.GlobalDryRun = false })

			ctx := context.Background()
			key := types.NamespacedName{Name: "global-dry-run-delete", Namespace: TestNamespace}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
					Annotations: map[string]string{
						DeleteAtAnnotation: time.Now().Add(-time.Minute).Format(time.RFC3339),
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "nginx"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, deployment)).To(Succeed())

			expectEventReason(ctx, TestNamespace, key.Name, "DryRunDelete")

			err := k8sClient.Get(ctx, key, &appsv1.Deployment{})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, deployment)).To(Succeed())
		})
	})
})

func sleepPastTimestampSecond() {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)) + 25*time.Millisecond)
}
