//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

func catchUpTests(namespaces *namespaceCleanup) {
	Context("Catch-up policy", func() {
		var namespace string
		const name = "catch-up-target"

		BeforeEach(func() {
			namespace = namespaces.create("lifecycle-e2e-catch-up")
		})

		DescribeTable("skips a stale one-time action", func(action, policy string) {
			applyCatchUpDeployment(namespace, map[string]string{
				"lifecycle.cezary.dev/" + action + "-at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
				"lifecycle.cezary.dev/catch-up":          policy,
			})

			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(name, namespace, g)
				g.Expect(dep.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/" + action + "-at"))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/catch-up"]).To(Equal(policy))
				g.Expect(dep.Spec.Template.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/restartedAt"))
				g.Expect(utils.GetEvents(g, namespace, name, "Deployment")).To(
					ContainElement(HaveField("Reason", "ActionSkipped")))
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())

			Consistently(func(g Gomega) {
				dep := utils.GetDeployment(name, namespace, g)
				g.Expect(dep.Spec.Template.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/restartedAt"))
			}).WithTimeout(2 * time.Second).WithPolling(pollInterval).Should(Succeed())
		},
			Entry("delete with never", "delete", "never"),
			Entry("restart with never", "restart", "never"),
			Entry("delete after its catch-up deadline", "delete", "15m"),
			Entry("restart after its catch-up deadline", "restart", "15m"),
		)

		DescribeTable("executes a recent missed action", func(action string) {
			applyCatchUpDeployment(namespace, map[string]string{
				"lifecycle.cezary.dev/" + action + "-at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				"lifecycle.cezary.dev/catch-up":          "15m",
			})
			expectCatchUpAction(name, namespace, action)
		},
			Entry("delete within its catch-up deadline", "delete"),
			Entry("restart within its catch-up deadline", "restart"),
		)

		DescribeTable("executes an armed action with never", func(action string) {
			applyCatchUpDeployment(namespace, map[string]string{
				"lifecycle.cezary.dev/" + action + "-at": time.Now().UTC().Add(6 * time.Second).Format(time.RFC3339),
				"lifecycle.cezary.dev/catch-up":          "never",
			})
			expectCatchUpAction(name, namespace, action)
		},
			Entry("delete at a future deadline", "delete"),
			Entry("restart at a future deadline", "restart"),
		)

		It("skips missed recurring restarts and executes the next armed occurrence", func() {
			anchor := time.Now().UTC().Add(-330 * time.Second).Format(time.RFC3339)
			applyCatchUpDeployment(namespace, map[string]string{
				"lifecycle.cezary.dev/restart-every":          "1m",
				"lifecycle.cezary.dev/last-restart-timestamp": anchor,
				"lifecycle.cezary.dev/catch-up":               "never",
			})

			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(name, namespace, g)
				g.Expect(dep.Annotations["lifecycle.cezary.dev/last-restart-timestamp"]).NotTo(Equal(anchor))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/restart-every"]).To(Equal("1m"))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/catch-up"]).To(Equal("never"))
				g.Expect(dep.Spec.Template.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/restartedAt"))
				g.Expect(utils.GetEvents(g, namespace, name, "Deployment")).To(
					ContainElement(HaveField("Reason", "ActionSkipped")))
			}).WithTimeout(10 * time.Second).WithPolling(pollInterval).Should(Succeed())

			Eventually(func(g Gomega) {
				dep := utils.GetDeployment(name, namespace, g)
				g.Expect(dep.Spec.Template.Annotations).To(HaveKey("lifecycle.cezary.dev/restartedAt"))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/restart-every"]).To(Equal("1m"))
				g.Expect(dep.Annotations["lifecycle.cezary.dev/catch-up"]).To(Equal("never"))
			}).WithTimeout(65 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})
	})
}

func applyCatchUpDeployment(namespace string, annotations map[string]string) {
	encoded, err := json.Marshal(annotations)
	Expect(err).NotTo(HaveOccurred())
	utils.ApplyYAML(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: catch-up-target
  namespace: %s
  annotations: %s
spec:
  replicas: 0
  selector: { matchLabels: { app: catch-up-target }}
  template:
    metadata: { labels: { app: catch-up-target }}
    spec: { containers: [ { name: nginx, image: nginx:latest } ] }
`, namespace, encoded))
}

func expectCatchUpAction(name, namespace, action string) {
	Eventually(func(g Gomega) {
		if action == "delete" {
			_, err := utils.Run(exec.Command("kubectl", "get", "deployment", name, "-n", namespace))
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("not found"))
			return
		}
		dep := utils.GetDeployment(name, namespace, g)
		g.Expect(dep.Annotations).NotTo(HaveKey("lifecycle.cezary.dev/restart-at"))
		g.Expect(dep.Spec.Template.Annotations).To(HaveKey("lifecycle.cezary.dev/restartedAt"))
	}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())
}
