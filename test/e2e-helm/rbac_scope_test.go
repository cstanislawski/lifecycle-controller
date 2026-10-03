//go:build e2e_helm

package e2e_helm

import (
	"fmt"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

func verifyNamespaceScopedRBAC(chartPath, release, manager string) {
	GinkgoHelper()
	const (
		allowedA = "e2e-allowed-ns"
		allowedB = "e2e-allowed-ns-b"
		denied   = "e2e-ignored-ns"
	)
	serviceAccount := "system:serviceaccount:" + manager + ":" + release + "-lifecycle-controller"
	for _, namespace := range []string{allowedA, allowedB, denied} {
		DeferCleanup(func() {
			_, err := utils.Run(exec.Command("kubectl", "delete", "namespace", namespace, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())
		})
		_, err := utils.Run(exec.Command("kubectl", "create", "namespace", namespace))
		Expect(err).NotTo(HaveOccurred())
	}
	DeferCleanup(func() {
		if CurrentSpecReport().Failed() {
			_, _ = utils.Run(exec.Command("kubectl", "logs", "deployment/"+release+"-lifecycle-controller", "-n", manager))
		}
	})
	image := strings.SplitN(projectImage, ":", 2)
	_, err := utils.Run(exec.Command("helm", "install", release, chartPath, "--namespace", manager,
		"--set", "image.repository="+image[0], "--set", "image.tag="+image[1],
		"--set", "image.pullPolicy=Never",
		"--set", "controllerManager.scope.watchResources={configmaps,secrets}",
		"--set", "controllerManager.scope.ignoreResources={secrets}",
		"--set", "controllerManager.scope.watchNamespaces={"+allowedA+","+allowedB+","+denied+"}",
		"--set", "controllerManager.scope.ignoreNamespaces={"+denied+"}"))
	Expect(err).NotTo(HaveOccurred())
	By("waiting for the scoped cache and leader election")
	_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+release+"-lifecycle-controller",
		"-n", manager, "--timeout=120s"))
	Expect(err).NotTo(HaveOccurred())

	By("checking effective permissions through the API server")
	for _, namespace := range []string{allowedA, allowedB} {
		checkScopedAccess(serviceAccount, "get", "secrets", namespace, "no")
		checkScopedAccess(serviceAccount, "create", "events", namespace, "yes")
	}
	checkScopedAccess(serviceAccount, "delete", "configmaps", denied, "no")
	checkScopedAccess(serviceAccount, "list", "configmaps", manager, "no")
	checkScopedAccess(serviceAccount, "get", "namespaces", "", "no")
	checkScopedAccess(serviceAccount, "create", "tokenreviews.authentication.k8s.io", "", "yes")
	checkScopedAccess(serviceAccount, "create", "subjectaccessreviews.authorization.k8s.io", "", "yes")
	output, err := utils.Run(exec.Command("kubectl", "get", "configmaps", "-n", denied, "--as="+serviceAccount))
	Expect(err).To(HaveOccurred())
	Expect(output).To(ContainSubstring("Forbidden"))
	output, err = utils.Run(exec.Command("kubectl", "get", "configmaps", "--all-namespaces", "--as="+serviceAccount))
	Expect(err).To(HaveOccurred())
	Expect(output).To(ContainSubstring("Forbidden"))

	By("checking lifecycle actions in both permitted namespaces")
	for _, namespace := range []string{allowedA, allowedB, denied} {
		utils.ApplyYAML(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: rbac-delete
  namespace: %s
  annotations:
    lifecycle.cezary.dev/delete-after: "2s"
data: {key: value}
`, namespace))
	}
	for _, namespace := range []string{allowedA, allowedB} {
		Eventually(func(g Gomega) {
			output, err := utils.Run(exec.Command("kubectl", "get", "configmap", "rbac-delete", "-n", namespace))
			g.Expect(err).To(HaveOccurred())
			g.Expect(output).To(ContainSubstring("NotFound"))
		}).WithTimeout(safeTimeout).WithPolling(pollInterval).Should(Succeed())
	}
	Consistently(func(g Gomega) {
		output, err := utils.Run(exec.Command("kubectl", "get", "configmap", "rbac-delete", "-n", denied,
			"-o", "jsonpath={.metadata.annotations}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(ContainSubstring("delete-after"))
		g.Expect(output).NotTo(ContainSubstring("delete-at"))
	}).WithTimeout(consistentDuration).WithPolling(pollInterval).Should(Succeed())
}

func checkScopedAccess(serviceAccount, verb, resource, namespace, want string) {
	args := []string{"auth", "can-i", verb, resource, "--as=" + serviceAccount}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	output, err := utils.Run(exec.Command("kubectl", args...))
	if want == "yes" {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	lines := utils.GetNonEmptyLines(output)
	ExpectWithOffset(1, lines).NotTo(BeEmpty())
	ExpectWithOffset(1, lines[len(lines)-1]).To(Equal(want), "%s %s in %s", verb, resource, namespace)
}
