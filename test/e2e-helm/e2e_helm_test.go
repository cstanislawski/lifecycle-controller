//go:build e2e_helm
// +build e2e_helm

package e2e_helm

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

const (
	releaseName       = "lc-helm-test"
	managerNamespace  = "lifecycle-helm-e2e"
	deploymentTimeout = 3 * time.Minute
)

var _ = Describe("Lifecycle Controller Helm E2E", Ordered, func() {
	var chartPath string

	BeforeAll(func() {
		By("getting project dir")
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		chartPath = filepath.Join(projectDir, "charts", "lifecycle-controller")

		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", managerNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("removing manager namespace")
		cmd := exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Default Helm Install", func() {
		var controllerPodName string

		It("should deploy successfully with default values", func() {
			By("installing the helm chart")
			repoAndTag := strings.SplitN(projectImage, ":", 2)
			repo, tag := repoAndTag[0], repoAndTag[1]

			cmd := exec.Command("helm", "install", releaseName, chartPath,
				"--namespace", managerNamespace,
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=Never", // Ensure it uses the local Kind image
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get pod name
				cmd := exec.Command("kubectl", "get", "pods",
					"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", releaseName),
					"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", managerNamespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1))
				controllerPodName = podNames[0]

				// Check pod status
				cmd = exec.Command("kubectl", "get", "pods", controllerPodName, "-o", "jsonpath={.status.phase}", "-n", managerNamespace)
				status, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("Running"))
			}
			Eventually(verifyControllerUp).WithTimeout(deploymentTimeout).WithPolling(time.Second).Should(Succeed())
		})

		It("should perform a basic lifecycle action (smoke test)", func() {
			By("creating a Deployment with a short delete-after annotation")
			deploymentName := "helm-e2e-delete-after"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "10s"
spec:
  replicas: 1
  selector: { matchLabels: { app: smoke-test } }
  template:
    metadata: { labels: { app: smoke-test } }
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, managerNamespace)

			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", managerNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
		})

		AfterAll(func() {
			By("uninstalling the helm chart")
			cmd := exec.Command("helm", "uninstall", releaseName, "--namespace", managerNamespace)
			_, _ = utils.Run(cmd)
		})
	})

	Context("Helm Install with Custom Values", func() {
		const customReleaseName = "lc-helm-custom"

		AfterEach(func() {
			By("uninstalling the custom helm chart")
			cmd := exec.Command("helm", "uninstall", customReleaseName, "--namespace", managerNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should deploy with leader election disabled", func() {
			By("installing the helm chart with leaderElection disabled")
			repoAndTag := strings.SplitN(projectImage, ":", 2)
			repo, tag := repoAndTag[0], repoAndTag[1]

			cmd := exec.Command("helm", "install", customReleaseName, chartPath,
				"--namespace", managerNamespace,
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=Never",
				"--set", "controllerManager.leaderElection.enabled=false",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the pod starts and does not have the --leader-elect flag")
			Eventually(func(g Gomega) {
				pod := utils.GetPodForRelease(g, customReleaseName, managerNamespace)
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))

				foundLeaderElect := false
				for _, arg := range pod.Spec.Containers[0].Args {
					if arg == "--leader-elect" {
						foundLeaderElect = true
						break
					}
				}
				g.Expect(foundLeaderElect).To(BeFalse(), "The --leader-elect flag should not be present in container args")
			}).WithTimeout(deploymentTimeout).WithPolling(time.Second).Should(Succeed())
		})
	})

	Context("HA Helm Install with Multiple Replicas", func() {
		const haReleaseName = "lc-helm-ha"

		AfterEach(func() {
			By("uninstalling the HA helm chart")
			cmd := exec.Command("helm", "uninstall", haReleaseName, "--namespace", managerNamespace)
			_, _ = utils.Run(cmd)
		})

		It("should elect a leader and remain functional", func() {
			By("installing the helm chart with replicaCount=2")
			repoAndTag := strings.SplitN(projectImage, ":", 2)
			repo, tag := repoAndTag[0], repoAndTag[1]

			cmd := exec.Command("helm", "install", haReleaseName, chartPath,
				"--namespace", managerNamespace,
				"--set", fmt.Sprintf("image.repository=%s", repo),
				"--set", fmt.Sprintf("image.tag=%s", tag),
				"--set", "image.pullPolicy=Never",
				"--set", "replicaCount=2",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying that both pods are running")
			Eventually(func(g Gomega) {
				pods := utils.GetPodsForRelease(g, haReleaseName, managerNamespace)
				g.Expect(pods).To(HaveLen(2))
				for _, pod := range pods {
					g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
				}
			}).WithTimeout(deploymentTimeout).WithPolling(time.Second).Should(Succeed())

			By("verifying a leader election lease exists and has a leader")
			Eventually(func(g Gomega) {
				// The lease name is generated by the helm chart's fullname helper
				leaseName := fmt.Sprintf("%s-lifecycle-controller", haReleaseName)
				lease := utils.GetLease(g, leaseName, managerNamespace)
				g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
				g.Expect(*lease.Spec.HolderIdentity).NotTo(BeEmpty())
			}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

			By("performing a smoke test with multiple replicas active")
			deploymentName := "helm-ha-delete-after"
			deploymentYAML := fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "15s"
spec:
  replicas: 1
  selector: { matchLabels: { app: smoke-test-ha } }
  template:
    metadata: { labels: { app: smoke-test-ha } }
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, deploymentName, managerNamespace)

			utils.ApplyYAML(deploymentYAML)

			By("verifying the deployment is eventually deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", deploymentName, "-n", managerNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("not found"))
			}).WithTimeout(40 * time.Second).WithPolling(2 * time.Second).Should(Succeed())
		})
	})
})
