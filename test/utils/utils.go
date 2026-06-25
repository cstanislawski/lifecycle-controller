package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	certmanagerVersion = "v1.18.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// WaitForRFC3339Tick waits until time.Now().Format(time.RFC3339) changes.
func WaitForRFC3339Tick() {
	nextSecond := time.Now().Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(nextSecond) + 20*time.Millisecond)
}

// GetProjectDir will return the directory where the project is.
func GetProjectDir() (string, error) {
	path, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return path, nil
		}

		parent := filepath.Dir(path)
		if parent == path {
			return "", errors.New("could not find project root containing go.mod")
		}
		path = parent
	}
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// GetEvents fetches events for a specific resource.
func GetEvents(g Gomega, namespace, resourceName, resourceKind string) []corev1.Event {
	cmd := exec.Command("kubectl", "get", "events", "-n", namespace,
		"--field-selector", fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=%s", resourceName, resourceKind),
		"-o", "json")
	output, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to fetch events for resource")

	var eventList corev1.EventList
	g.Expect(json.Unmarshal([]byte(output), &eventList)).To(Succeed())
	return eventList.Items
}

// ApplyYAML is a helper function to apply a YAML string using kubectl.
func ApplyYAML(yamlContent string) {
	tmpFile, err := os.CreateTemp("", "e2e-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	defer func() {
		if err := os.Remove(tmpFile.Name()); err != nil {
			warnError(fmt.Errorf("failed to remove temporary file %s: %w", tmpFile.Name(), err))
		}
	}()

	_, err = tmpFile.WriteString(yamlContent)
	Expect(err).NotTo(HaveOccurred())
	Expect(tmpFile.Close()).To(Succeed())

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	_, err = Run(cmd)
	// Tolerate 'already exists' for idempotency if needed, but for clean tests, this should succeed.
	if err != nil && !strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "unchanged") {
		Fail(fmt.Sprintf("Failed to apply YAML: %v", err))
	}
}

// GetDeployment is a helper to fetch a deployment and unmarshal it, failing the test on error.
func GetDeployment(name, namespace string, g Gomega) *appsv1.Deployment {
	cmd := exec.Command("kubectl", "get", "deployment", name, "-n", namespace, "-o", "json")
	output, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	var dep appsv1.Deployment
	g.Expect(json.Unmarshal([]byte(output), &dep)).To(Succeed())
	return &dep
}

// GetPodForRelease is a helper to fetch the first pod for a given helm release.
func GetPodForRelease(g Gomega, releaseName, namespace string) *corev1.Pod {
	pods := GetPodsForRelease(g, releaseName, namespace)
	g.Expect(pods).To(HaveLen(1), "expected 1 running pod for the helm release")
	return &pods[0]
}

// GetPod is a helper to fetch a pod by name and unmarshal it.
func GetPod(g Gomega, name, namespace string) *corev1.Pod {
	cmd := exec.Command("kubectl", "get", "pod", name, "-n", namespace, "-o", "json")
	output, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	var pod corev1.Pod
	g.Expect(json.Unmarshal([]byte(output), &pod)).To(Succeed())
	return &pod
}

// GetPodsForRelease is a helper to fetch all pods for a given helm release.
func GetPodsForRelease(g Gomega, releaseName, namespace string) []corev1.Pod {
	cmd := exec.Command("kubectl", "get", "pods",
		"-l", fmt.Sprintf("app.kubernetes.io/instance=%s", releaseName),
		"-o", "json",
		"-n", namespace,
	)
	podOutput, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	var podList corev1.PodList
	g.Expect(json.Unmarshal([]byte(podOutput), &podList)).To(Succeed())

	// Manually filter out terminating pods
	var activePods []corev1.Pod
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp == nil {
			activePods = append(activePods, pod)
		}
	}
	return activePods
}

// GetLease is a helper to fetch a Lease object by name.
func GetLease(g Gomega, name, namespace string) *coordinationv1.Lease {
	cmd := exec.Command("kubectl", "get", "lease", name, "-n", namespace, "-o", "json")
	output, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred())

	var lease coordinationv1.Lease
	g.Expect(json.Unmarshal([]byte(output), &lease)).To(Succeed())
	return &lease
}

// GetLogs fetches the logs from a specific pod.
func GetLogs(g Gomega, podName, namespace string) string {
	cmd := exec.Command("kubectl", "logs", podName, "-n", namespace)
	output, err := Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "Failed to fetch logs for pod %s", podName)
	return output
}
