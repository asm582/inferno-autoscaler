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
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	utilruntime.Must(promoperator.AddToScheme(scheme))
}

// initializeK8sClient initializes the Kubernetes client for testing
func initializeK8sClient() (client.Client, error) {
	cfg, err := func() (*rest.Config, error) {
		if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
			return clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
		return rest.InClusterConfig()
	}()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return c, nil
}

var _ = Describe("WorkloadVariantAutoscaler Predictive Engine E2E", Ordered, func() {
	var (
		namespace string = "test-predictive-" + time.Now().Format("20060102150405")
		ctx       context.Context
		cancel    context.CancelFunc
		k8sClient client.Client
	)

	BeforeAll(func() {
		ctx, cancel = context.WithCancel(context.Background())
		var err error
		k8sClient, err = initializeK8sClient()
		Expect(err).NotTo(HaveOccurred())

		By("Creating namespace " + namespace)
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterAll(func() {
		By("Deleting namespace " + namespace)
		if k8sClient != nil {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		}
		cancel()
	})

	Context("Predictive Scaling Logic", func() {
		It("should scale up when predicted latency exceeds SLO", func() {
			modelID := "llama-2-70b"
			acceleratorName := "nvidia-a100"
			vaName := "predictive-va"
			deployName := "predictive-deploy"

			// 1. Create Deployment (start with 1 replica)
			// Use llmd-sim image as requested
			deploy := utils.CreateLlmdSimDeployment(
				namespace,
				deployName,
				modelID,
				"predictive-app",
				"8000", // port
				100,    // avgTTFT
				20,     // avgITL
				1,      // replicas
			)
			Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

			// 2. Create Service and ServiceMonitor (mock metrics)
			// We won't actually run a real load generator or Prometheus here since we mocked the Predictor client in main.go
			// However, we DO need a ServiceMonitor so the MetricsCollector considers the VA "valid" for inspection.

			// Create Service (ClusterIP to avoid NodePort collisions)
			svcLabels := map[string]string{
				"app":                       "predictive-app",
				"llm-d.ai/inferenceServing": "true",
			}
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "predictive-app",
					Namespace: namespace,
					Labels:    svcLabels,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port:       80,
						TargetPort: intstr.FromInt(8000), // llmd-sim runs on 8000
						Protocol:   corev1.ProtocolTCP,
						Name:       "http",
					}},
					Selector: svcLabels,
					Type:     corev1.ServiceTypeClusterIP,
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			sm := utils.CreateLlmdSimServiceMonitor(deployName, namespace, namespace, "predictive-app")
			Expect(k8sClient.Create(ctx, sm)).To(Succeed())

			// 3. Create VariantAutoscaling
			va := utils.CreateVariantAutoscalingResource(namespace, vaName, modelID, acceleratorName, 1.0)
			// Point to our deployment
			va.Spec.ScaleTargetRef.Name = deployName
			va.Spec.ScaleTargetRef.Kind = "Deployment"
			va.Spec.ScaleTargetRef.APIVersion = "apps/v1"

			// Set custom SLO
			maxITL := int32(20)
			va.Spec.SLO = &llmdVariantAutoscalingV1alpha1.ServiceLevelObjectives{
				MaxITL: &maxITL,
			}

			Expect(k8sClient.Create(ctx, va)).To(Succeed())

			// 4. Wait for Scaling Decision
			// The Mock Predictor in main.go returns ITL=25.0, Target=20.0
			// Engine should see this and calculate DesiredReplicas = 2 (1 + 1)

			Eventually(func() int {
				updatedVA := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: vaName, Namespace: namespace}, updatedVA)
				if err != nil {
					return -1
				}
				return updatedVA.Status.DesiredOptimizedAlloc.NumReplicas
			}, 5*time.Minute, 5*time.Second).Should(Equal(2), "Expected Predictive Engine to scale up to 2 replicas")
		})
	})
})
