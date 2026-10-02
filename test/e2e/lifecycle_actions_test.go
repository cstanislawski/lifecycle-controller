//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

func lifecycleActionTests(namespaces *namespaceCleanup) {
	Context("Lifecycle Actions", func() {
		var testNamespace string

		BeforeEach(func() {
			testNamespace = namespaces.create("lifecycle-e2e-tests")
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
  replicas: 1
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
  replicas: 1
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
			time.Sleep(1500 * time.Millisecond)

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
			deleteAtTime := time.Now().Add(6 * time.Second).UTC().Format(time.RFC3339)

			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-at: "%s"
spec:
  replicas: 1
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
			time.Sleep(1 * time.Second)
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
    lifecycle.cezary.dev/delete-after: "8s"
spec:
  replicas: 1
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
			time.Sleep(1 * time.Second)
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
  replicas: 1
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
  replicas: 1
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
			time.Sleep(1500 * time.Millisecond)

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

}
