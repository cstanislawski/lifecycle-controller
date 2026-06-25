//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	// projectImage is the name of the image which will be built and loaded
	// with the code source changes to be tested.
	projectImage string
)

func init() {
	// Read the image name from the environment variable, panic if not set
	projectImage = os.Getenv("IMG")
	if projectImage == "" {
		panic("IMG environment variable is not set")
	}
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting lifecycle-controller E2E test suite\n")
	RunSpecs(t, "E2E Suite")
}
