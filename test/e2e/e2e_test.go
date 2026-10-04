//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

var managerNamespace string

func init() {
	managerNamespace = os.Getenv("NAMESPACE")
	if managerNamespace == "" {
		panic("NAMESPACE environment variable must be set for E2E tests")
	}
}

const pollInterval = 250 * time.Millisecond

var _ = Describe("Lifecycle Controller E2E", Ordered, func() {
	var controllerPodName string
	namespaces := &namespaceCleanup{}

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", managerNamespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	AfterAll(func() {
		By("waiting for test namespace cleanup")
		cleanupErr := namespaces.wait(namespaceCleanupTimeout)

		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		Expect(cleanupErr).To(Succeed())
	})

	Context("Controller Health", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", "app.kubernetes.io/component=controller-manager",
					"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", managerNamespace,
				)
				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to get controller-manager pod name")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]

				cmd = exec.Command("kubectl", "get", "pods", controllerPodName, "-o", "jsonpath={.status.phase}", "-n", managerNamespace)
				status, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("Running"), "Controller manager pod is not running")
			}
			Eventually(verifyControllerUp).WithTimeout(3 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})

	lifecycleActionTests(namespaces)
	catchUpTests(namespaces)

	Context("Recurring and Advanced Actions", func() {
		var testNamespace string

		BeforeEach(func() {
			testNamespace = namespaces.create("lifecycle-e2e-advanced")
		})

		It("should coalesce missed 'restart-every' occurrences into one rollout", func() {
			deploymentName := "e2e-restart-every"
			lastRestart := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector: { matchLabels: { app: test-restart-every }}
  template:
    metadata: { labels: { app: test-restart-every }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace)
			utils.ApplyYAML(deploymentYAML)

			By("applying the recurring schedule to the existing deployment")
			cmd := exec.Command(
				"kubectl", "annotate", "deployment", deploymentName, "-n", testNamespace,
				"lifecycle.cezary.dev/restart-every=1m",
				fmt.Sprintf("lifecycle.cezary.dev/last-restart-timestamp=%s", lastRestart),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			var restartedAt, coalescedAnchor string
			By("verifying one coalesced restart happens")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				g.Expect(dep.Annotations).To(HaveKey("lifecycle.cezary.dev/last-restart-timestamp"))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]).NotTo(Equal(lastRestart))
				var found bool
				restartedAt, found = dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue(), "coalesced restart should have occurred")
				coalescedAnchor = dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying missed occurrences do not trigger catch-up rollouts")
			Consistently(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				currentRestartedAt, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue())
				g.Expect(currentRestartedAt).To(Equal(restartedAt))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]).To(Equal(coalescedAnchor))
			}).WithTimeout(5 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should use creationTimestamp for 'delete-after' when specified", func() {
			deploymentName := "e2e-ref-point-creation"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "3s"
    lifecycle.cezary.dev/reference-point: "creationTimestamp"
spec:
  replicas: 1
  selector: { matchLabels: { app: test-ref-point }}
  template:
    metadata: { labels: { app: test-ref-point }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace)
			utils.ApplyYAML(deploymentYAML)

			// Get the resource immediately to capture its creation timestamp
			dep := utils.GetDeployment(deploymentName, testNamespace, NewGomegaWithT(GinkgoT()))
			creationTimestamp := dep.GetCreationTimestamp().Time.UTC()

			By("re-applying the manifest with a longer duration")
			time.Sleep(1 * time.Second)
			deploymentYAML = strings.Replace(deploymentYAML, `delete-after: "3s"`, `delete-after: "1h"`, 1)
			utils.ApplyYAML(deploymentYAML)

			By("verifying the 'delete-at' is based on the original creationTimestamp")
			Eventually(func(g Gomega) {
				updatedDep := utils.GetDeployment(deploymentName, testNamespace, g)
				deleteAtStr, found := updatedDep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue())
				deleteAt, err := time.Parse(time.RFC3339, deleteAtStr)
				g.Expect(err).NotTo(HaveOccurred())
				// The delete time should be ~3s after creation, not affected by the re-apply
				g.Expect(deleteAt).To(BeTemporally("~", creationTimestamp.Add(3*time.Second), 2*time.Second))
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("verifying the deployment is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should support cron-timezone for restart-cron", func() {
			deploymentName := "e2e-cron-timezone"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/restart-cron: "* * * * *"
    lifecycle.cezary.dev/cron-timezone: "America/Los_Angeles"
spec:
  replicas: 1
  selector: { matchLabels: { app: test-timezone }}
  template:
    metadata: { labels: { app: test-timezone }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace)
			utils.ApplyYAML(deploymentYAML)

			By("verifying the recurring restart state is initialized")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				_, found := dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]
				g.Expect(found).To(BeTrue())
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying the deployment is restarted")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				_, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue())
			}).WithTimeout(90 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})

	Context("Cross-Resource Type and Error Handling", func() {
		It("should delete a Namespace and its contents", func() {
			namespaceName := namespaces.reserve("e2e-ns-to-delete")
			DeferCleanup(func() { Expect(namespaces.delete(namespaceName)).To(Succeed()) })
			deleteAtTime := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)

			namespaceYAML := fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
  annotations:
    lifecycle.cezary.dev/delete-at: "%s"
`, namespaceName, deleteAtTime)
			utils.ApplyYAML(namespaceYAML)

			configMapYAML := fmt.Sprintf(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: %s
data:
  key: value
`, namespaceName)
			utils.ApplyYAML(configMapYAML)

			By("verifying the namespace is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "namespace", namespaceName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should ignore conflicting delete and restart annotations and post an event", func() {
			testNamespace := namespaces.create("lifecycle-e2e-conflict")
			deploymentName := "e2e-conflict"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "1s"
    lifecycle.cezary.dev/restart-after: "1s"
spec:
  replicas: 1
  selector: { matchLabels: { app: test-conflict }}
  template:
    metadata: { labels: { app: test-conflict }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace)
			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is NOT deleted or restarted")
			Consistently(func(g Gomega) {
				// Check it still exists
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Check it was not restarted
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				_, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeFalse())
			}).WithTimeout(5 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying a warning event was posted")
			Eventually(func(g Gomega) {
				events := utils.GetEvents(g, testNamespace, deploymentName, "Deployment")
				g.Expect(events).NotTo(BeEmpty())
				foundEvent := false
				for _, event := range events {
					if event.Reason == "ConflictingAnnotations" && event.Type == "Warning" {
						foundEvent = true
						break
					}
				}
				g.Expect(foundEvent).To(BeTrue(), "expected to find a ConflictingAnnotations warning event")
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should not perform actions when 'dry-run' is true", func() {
			testNamespace := namespaces.create("lifecycle-e2e-dry-run")
			deploymentName := "e2e-dry-run"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "2s"
    lifecycle.cezary.dev/dry-run: "true"
spec:
  replicas: 1
  selector: { matchLabels: { app: test-dry-run }}
  template:
    metadata: { labels: { app: test-dry-run }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace)
			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is NOT deleted")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).WithTimeout(5 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying a dry-run event was posted")
			Eventually(func(g Gomega) {
				events := utils.GetEvents(g, testNamespace, deploymentName, "Deployment")
				g.Expect(events).NotTo(BeEmpty(), "expected events for dry-run")
				foundEvent := false
				for _, event := range events {
					if event.Reason == "DryRunDelete" {
						foundEvent = true
						break
					}
				}
				g.Expect(foundEvent).To(BeTrue(), "expected to find DryRunDelete event")
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})

	Context("Dynamic CRD Handling", func() {
		var testNamespace string
		const crdName = "lifecyclee2etests.testing.lifecycle.cezary.dev"
		const crdYAML = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ` + crdName + `
spec:
  group: testing.lifecycle.cezary.dev
  names:
    kind: LifecycleE2ETest
    listKind: LifecycleE2ETestList
    plural: lifecyclee2etests
    singular: lifecyclee2etest
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            x-kubernetes-preserve-unknown-fields: true
          status:
            type: object
            x-kubernetes-preserve-unknown-fields: true
`

		BeforeAll(func() {
			By("creating the test CRD")
			utils.ApplyYAML(crdYAML)

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "crd", crdName)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("restarting the controller to discover the new CRD")
			cmd := exec.Command("kubectl", "delete", "pod", "-n", managerNamespace, "-l", "app.kubernetes.io/component=controller-manager")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd = exec.Command("kubectl", "wait", "pod", "-n", managerNamespace, "-l", "app.kubernetes.io/component=controller-manager", "--for=condition=Ready", "--timeout=2m")
				_, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).WithTimeout(3 * time.Minute).WithPolling(pollInterval).Should(Succeed())
		})

		AfterAll(func() {
			// Keep the CRD available until namespace finalization completes.
			By("waiting for namespace cleanup before removing the test CRD")
			cleanupErr := namespaces.wait(namespaceCleanupTimeout)
			By("deleting the test CRD")
			cmd := exec.Command("kubectl", "delete", "crd", crdName, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			Expect(cleanupErr).To(Succeed())
		})

		BeforeEach(func() {
			testNamespace = namespaces.create("lifecycle-e2e-crd")
		})

		It("should delete a custom resource instance after the 'delete-after' duration", func() {
			crName := "e2e-cr-delete-after"
			crYAML := fmt.Sprintf(`
apiVersion: testing.lifecycle.cezary.dev/v1alpha1
kind: LifecycleE2ETest
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "3s"
spec:
  foo: bar
`, crName, testNamespace)
			utils.ApplyYAML(crYAML)

			By("verifying the custom resource is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lifecyclee2etest", crName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})
})
