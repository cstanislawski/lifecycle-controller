//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

const namespaceCleanupTimeout = 2 * time.Minute

type namespaceCleanup struct {
	names []string
}

func (c *namespaceCleanup) reserve(prefix string) string {
	name := prefix + "-" + string(uuid.NewUUID())
	c.names = append(c.names, name)
	return name
}

func (c *namespaceCleanup) create(prefix string) string {
	name := c.reserve(prefix)
	// Register cleanup before creation. An API response can be lost after creation.
	DeferCleanup(func() {
		By("requesting namespace cleanup: " + name)
		Expect(c.delete(name)).To(Succeed())
	})
	By("creating test namespace: " + name)
	_, err := utils.Run(exec.Command("kubectl", "create", "namespace", name, "--request-timeout=30s"))
	Expect(err).NotTo(HaveOccurred())
	return name
}

func (c *namespaceCleanup) delete(names ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"delete", "namespace"}, names...)
	args = append(args, "--ignore-not-found", "--wait=false", "--request-timeout=30s")
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", args...))
	return err
}

func (c *namespaceCleanup) wait(timeout time.Duration) error {
	if len(c.names) == 0 {
		return nil
	}
	// Retry requests that failed during case cleanup before waiting for all namespaces.
	if err := c.delete(c.names...); err != nil {
		return c.failure(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := append([]string{"wait", "--for=delete", "namespace"}, c.names...)
	args = append(args, "--timeout="+timeout.String())
	_, err := utils.Run(exec.CommandContext(ctx, "kubectl", args...))
	if err != nil {
		return c.failure(err)
	}
	c.names = nil
	return nil
}

func (c *namespaceCleanup) failure(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var diagnostics strings.Builder
	args := append([]string{"get", "namespace"}, c.names...)
	args = append(args, "--ignore-not-found", "-o", "yaml")
	output, err := utils.Run(exec.CommandContext(ctx, "kubectl", args...))
	_, _ = fmt.Fprintf(&diagnostics, "Namespaces:\n%s\nError: %v\n", output, err)
	for _, name := range c.names {
		if ctx.Err() != nil {
			break
		}
		output, err = utils.Run(exec.CommandContext(ctx, "kubectl", "get", "pods", "-n", name, "-o", "yaml"))
		_, _ = fmt.Fprintf(&diagnostics, "Pods in %s:\n%s\nError: %v\n", name, output, err)
	}
	return fmt.Errorf("namespace cleanup failed for %v: %w\n%s", c.names, cause, diagnostics.String())
}
