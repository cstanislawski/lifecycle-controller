//go:build e2e
// +build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

func TestNamespaceCleanup(t *testing.T) {
	cleanup := &namespaceCleanup{}
	first := cleanup.reserve("lifecycle-e2e-cleanup")
	second := cleanup.reserve("lifecycle-e2e-cleanup")
	if first == second {
		t.Fatal("test namespaces must be unique")
	}
	run := func(args ...string) string {
		t.Helper()
		output, err := utils.Run(exec.Command("kubectl", args...))
		if err != nil {
			t.Fatal(err)
		}
		return output
	}
	t.Cleanup(func() {
		// Release the probe finalizer even if an assertion fails.
		_, _ = utils.Run(exec.Command("kubectl", "patch", "configmap", "cleanup-probe", "-n", first,
			"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`, "--request-timeout=10s"))
		if err := cleanup.wait(30 * time.Second); err != nil {
			t.Error(err)
		}
	})
	run("create", "namespace", first, "--request-timeout=10s")
	run("create", "configmap", "cleanup-probe", "-n", first, "--from-literal=key=value")
	run("patch", "configmap", "cleanup-probe", "-n", first, "--type=merge", "-p",
		`{"metadata":{"finalizers":["testing.lifecycle.cezary.dev/cleanup-probe"]}}`)
	if err := cleanup.delete(first); err != nil {
		t.Fatal(err)
	}
	if run("get", "namespace", first, "-o", "jsonpath={.metadata.deletionTimestamp}") == "" {
		t.Fatal("cleanup must request deletion before it returns")
	}
	// The first namespace cannot finish deletion while its resource has a finalizer.
	run("create", "namespace", second, "--request-timeout=10s")
	run("create", "configmap", "cleanup-probe", "-n", second, "--from-literal=key=second")
	if run("get", "configmap", "cleanup-probe", "-n", second, "-o", "jsonpath={.data.key}") != "second" {
		t.Fatal("subsequent cases must use isolated resources while cleanup is pending")
	}
	run("get", "configmap", "cleanup-probe", "-n", first)
	err := cleanup.wait(time.Second)
	if err == nil || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), "Namespaces:") {
		t.Fatalf("blocked cleanup must fail with namespace diagnostics, got %v", err)
	}
	if len(cleanup.names) != 2 {
		t.Fatal("failed cleanup must retain namespaces for retry")
	}
	run("patch", "configmap", "cleanup-probe", "-n", first, "--type=merge", "-p",
		`{"metadata":{"finalizers":[]}}`)
	if err := cleanup.wait(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if len(cleanup.names) != 0 {
		t.Fatal("successful cleanup must drain all tracked namespaces")
	}
	output := run("get", "namespace", first, second, "--ignore-not-found", "-o", "name")
	if strings.TrimSpace(output) != "" {
		t.Fatalf("cleanup left namespaces in the cluster: %s", output)
	}
	// An API create failure can leave a tracked name that never existed.
	cleanup.reserve("lifecycle-e2e-missing")
	if err := cleanup.wait(time.Second); err != nil {
		t.Fatalf("cleanup must accept a namespace that is already absent: %v", err)
	}
}
