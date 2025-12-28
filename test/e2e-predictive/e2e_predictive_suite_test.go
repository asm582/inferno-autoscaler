/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e_predictive_test

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var (
	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "ghcr.io/llm-d-incubation/workload-variant-autoscaler:0.0.1-test"
)

const (
	maximumAvailableGPUs = 4
	numNodes             = 3
	gpuTypes             = "mix"
)

func TestPredictiveE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting workload-variant-autoscaler predictive engine test suite\n")
	RunSpecs(t, "e2e predictive engine suite")
}

var _ = BeforeSuite(func() {
	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	By("exporting environment variables for deployment")
	utils.SetupTestEnvironment(projectImage, numNodes, maximumAvailableGPUs, gpuTypes)

	// Deploy llm-d and workload-variant-autoscaler on the Kind cluster
	By("deploying Workload Variant Autoscaler on Kind")
	launchCmd := exec.Command("make", "deploy-wva-emulated-on-kind", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(launchCmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install llm-d and workload-variant-autoscaler")
})
