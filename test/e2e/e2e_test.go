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
		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
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

	Context("Lifecycle Actions", func() {
		const testNamespace = "lifecycle-e2e-tests"

		BeforeEach(func() {
			By("creating test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("deleting test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should delete a Deployment after the 'delete-after' duration", func() {
			By("creating a Deployment with a short delete-after annotation")
			deploymentName := "e2e-delete-after"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "2s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-delete
  template:
    metadata:
      labels:
        app: test-delete
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace)

			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should reset the 'delete-after' timer on re-apply", func() {
			By("creating a Deployment with a 'delete-after' annotation")
			deploymentName := "e2e-delete-after-reset"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "4s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-delete-reset
  template:
    metadata:
      labels:
        app: test-delete-reset
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace)

			utils.ApplyYAML(deploymentYAML)

			var firstDeleteAt time.Time
			By("verifying the initial 'delete-at' annotation is set")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				deleteAtStr, found := dep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue(), "'delete-at' annotation should be present")

				var err error
				firstDeleteAt, err = time.Parse(time.RFC3339, deleteAtStr)
				g.Expect(err).NotTo(HaveOccurred())
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("waiting a moment before re-applying")
			utils.WaitForRFC3339Tick()

			By("re-applying the same manifest to reset the timer")
			utils.ApplyYAML(deploymentYAML)

			By("verifying the 'delete-at' annotation is updated to a later time")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				newDeleteAtStr, found := dep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue())

				newDeleteAt, err := time.Parse(time.RFC3339, newDeleteAtStr)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(newDeleteAt.After(firstDeleteAt)).To(BeTrue(), "The new deletion time should be later than the original")
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())
		})

		It("should not change a fixed 'delete-at' time on re-apply", func() {
			By("creating a Deployment with a fixed 'delete-at' annotation")
			deploymentName := "e2e-delete-at-fixed"
			deleteAtTime := time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339)

			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-at: "%s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-delete-fixed
  template:
    metadata:
      labels:
        app: test-delete-fixed
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace, deleteAtTime)

			utils.ApplyYAML(deploymentYAML)

			By("re-applying the same manifest after a delay")
			utils.WaitForRFC3339Tick()
			utils.ApplyYAML(deploymentYAML)

			By("verifying the 'delete-at' annotation has not changed")
			// We use Consistently here to ensure it doesn't flap
			Consistently(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				currentDeleteAt, found := dep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue())
				g.Expect(currentDeleteAt).To(Equal(deleteAtTime))
			}).WithTimeout(2 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying the deployment is eventually deleted at the fixed time")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should not update the 'delete-at' time on unrelated resource updates", func() {
			By("creating a Deployment with a 'delete-after' annotation")
			deploymentName := "e2e-unrelated-update"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "5s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-unrelated
  template:
    metadata:
      labels:
        app: test-unrelated
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace)

			utils.ApplyYAML(deploymentYAML)

			var initialDeleteAt string
			By("verifying the initial 'delete-at' annotation is set")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				deleteAtStr, found := dep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue(), "'delete-at' annotation should be present")
				g.Expect(dep.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/delete-after"), "'delete-after' should be removed")
				initialDeleteAt = deleteAtStr
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("making an unrelated update to the deployment (adding an annotation)")
			utils.WaitForRFC3339Tick()
			cmd := exec.Command("kubectl", "annotate", "deployment", deploymentName, "-n", testNamespace, "unrelated.io/test=true")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the 'delete-at' annotation has not changed")
			Consistently(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				currentDeleteAt, found := dep.Annotations["lifecycle.cezary.dev/delete-at"]
				g.Expect(found).To(BeTrue())
				g.Expect(currentDeleteAt).To(Equal(initialDeleteAt))
			}).WithTimeout(2 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying the deployment is eventually deleted at the original fixed time")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(15 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should restart a Deployment when 'restart-at' is in the past", func() {
			By("creating a Deployment with a past restart-at annotation")
			deploymentName := "e2e-restart-at"
			pastTime := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)

			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/restart-at: "%s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-restart
  template:
    metadata:
      labels:
        app: test-restart
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace, pastTime)

			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is restarted and annotations are updated")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)

				// Check that the main annotation is gone
				_, found := dep.Annotations["lifecycle.cezary.dev/restart-at"]
				g.Expect(found).To(BeFalse(), "restart-at annotation should be removed")

				// Check that the template annotation was added
				_, found = dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue(), "restartedAt annotation should be added to the template")

			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())
		})

		It("should reset the 'restart-after' timer on re-apply", func() {
			By("creating a Deployment with a 'restart-after' annotation")
			deploymentName := "e2e-restart-after-reset"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/restart-after: "4s"
spec:
  replicas: 0
  selector:
    matchLabels:
      app: test-restart-reset
  template:
    metadata:
      labels:
        app: test-restart-reset
    spec:
      containers:
      - name: nginx
        image: nginx:latest
`, deploymentName, testNamespace)

			utils.ApplyYAML(deploymentYAML)

			var firstRestartAt time.Time
			By("verifying the initial 'restart-at' annotation is set")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				restartAtStr, found := dep.Annotations["lifecycle.cezary.dev/restart-at"]
				g.Expect(found).To(BeTrue(), "'restart-at' annotation should be present")

				var err error
				firstRestartAt, err = time.Parse(time.RFC3339, restartAtStr)
				g.Expect(err).NotTo(HaveOccurred())
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())

			By("waiting a moment before re-applying")
			utils.WaitForRFC3339Tick()

			By("re-applying the same manifest to reset the timer")
			utils.ApplyYAML(deploymentYAML)

			By("verifying the 'restart-at' annotation is updated to a later time")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				newRestartAtStr, found := dep.Annotations["lifecycle.cezary.dev/restart-at"]
				g.Expect(found).To(BeTrue())

				newRestartAt, err := time.Parse(time.RFC3339, newRestartAtStr)
				g.Expect(err).NotTo(HaveOccurred())

				g.Expect(newRestartAt.After(firstRestartAt)).To(BeTrue(), "The new restart time should be later than the original")
			}).WithTimeout(time.Minute).WithPolling(pollInterval).Should(Succeed())
		})
	})

	Context("Recurring and Advanced Actions", func() {
		const testNamespace = "lifecycle-e2e-advanced"

		BeforeEach(func() {
			By("creating advanced test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("deleting advanced test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should restart a Deployment periodically based on 'restart-every'", func() {
			deploymentName := "e2e-restart-every"
			staleLastRestart := time.Now().Add(-3 * time.Second).UTC().Format(time.RFC3339)
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/restart-every: "1s"
    lifecycle.cezary.dev/last-restart-timestamp: "%s"
spec:
  replicas: 0
  selector: { matchLabels: { app: test-restart-every }}
  template:
    metadata: { labels: { app: test-restart-every }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace, staleLastRestart)
			utils.ApplyYAML(deploymentYAML)

			var firstLastRestart string
			By("verifying the first restart happens")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				lastRestart := dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]
				g.Expect(lastRestart).NotTo(Equal(staleLastRestart))
				restartedAt, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue(), "first restart should have occurred")
				g.Expect(restartedAt).NotTo(BeEmpty())
				firstLastRestart = lastRestart
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())

			By("verifying a second restart happens, showing recurring logic works")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				lastRestart := dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]
				g.Expect(lastRestart).NotTo(Equal(firstLastRestart), "a second recurring schedule tick should have been recorded")
				_, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue())
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())
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
  replicas: 0
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
			utils.WaitForRFC3339Tick()
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
			staleLastRestart := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/restart-cron: "* * * * *"
    lifecycle.cezary.dev/cron-timezone: "America/Los_Angeles"
    lifecycle.cezary.dev/last-restart-timestamp: "%s"
spec:
  replicas: 0
  selector: { matchLabels: { app: test-timezone }}
  template:
    metadata: { labels: { app: test-timezone }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, testNamespace, staleLastRestart)
			utils.ApplyYAML(deploymentYAML)

			By("verifying cron-timezone scheduling triggers from stale recurring state")
			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(deploymentName, testNamespace, g)
				lastRestart := dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]
				g.Expect(lastRestart).NotTo(Equal(staleLastRestart))
				_, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
				g.Expect(found).To(BeTrue())
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})

	Context("Cross-Resource Type and Error Handling", func() {
		It("should delete a Namespace and its contents", func() {
			namespaceName := "e2e-ns-to-delete"

			namespaceYAML := fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, namespaceName)
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

			By("annotating the namespace for immediate deletion")
			deleteAtTime := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			cmd := exec.Command("kubectl", "annotate", "namespace", namespaceName, fmt.Sprintf("lifecycle.cezary.dev/delete-at=%s", deleteAtTime))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the namespace is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "namespace", namespaceName)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(20 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("should ignore conflicting delete and restart annotations and post an event", func() {
			deploymentName := "e2e-conflict"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
  annotations:
    lifecycle.cezary.dev/delete-after: "1s"
    lifecycle.cezary.dev/restart-after: "1s"
spec:
  replicas: 0
  selector: { matchLabels: { app: test-conflict }}
  template:
    metadata: { labels: { app: test-conflict }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName)
			utils.ApplyYAML(deploymentYAML)

			By("verifying a warning event was posted")
			Eventually(func(g Gomega) {
				events := utils.GetEvents(g, "default", deploymentName, "Deployment")
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

			By("verifying the deployment is NOT deleted or restarted")
			cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", "default")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			dep := utils.GetDeployment(deploymentName, "default", NewGomegaWithT(GinkgoT()))
			_, found := dep.Spec.Template.Annotations["lifecycle.cezary.dev/restartedAt"]
			Expect(found).To(BeFalse())

			cmd = exec.Command("kubectl", "delete", "deployment", deploymentName, "-n", "default")
			_, _ = utils.Run(cmd)
		})

		It("should not perform actions when 'dry-run' is true", func() {
			deploymentName := "e2e-dry-run"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: default
  annotations:
    lifecycle.cezary.dev/delete-after: "2s"
    lifecycle.cezary.dev/dry-run: "true"
spec:
  replicas: 0
  selector: { matchLabels: { app: test-dry-run }}
  template:
    metadata: { labels: { app: test-dry-run }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName)
			utils.ApplyYAML(deploymentYAML)

			By("verifying a dry-run event was posted")
			Eventually(func(g Gomega) {
				events := utils.GetEvents(g, "default", deploymentName, "Deployment")
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

			By("verifying the deployment is NOT deleted")
			cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", "default")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Cleanup
			cmd = exec.Command("kubectl", "delete", "deployment", deploymentName, "-n", "default")
			_, _ = utils.Run(cmd)
		})
	})

	Context("Dynamic CRD Handling", func() {
		const testNamespace = "lifecycle-e2e-crd"
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
			By("deleting the test CRD")
			cmd := exec.Command("kubectl", "delete", "crd", crdName, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		BeforeEach(func() {
			By("creating CRD test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("deleting CRD test namespace")
			cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
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
    lifecycle.cezary.dev/delete-at: "%s"
spec:
  foo: bar
`, crName, testNamespace, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
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
