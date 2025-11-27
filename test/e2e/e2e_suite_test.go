//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cstanislawski/lifecycle-controller/test/utils"
)

var (
	// projectImage is the name of the image which will be built and loaded
	// with the code source changes to be tested.
	projectImage string
)

func init() {
	// Read the image name from the environment variable, or default if not set
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

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")
})
