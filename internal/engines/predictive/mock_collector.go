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

package predictive

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	appsv1 "k8s.io/api/apps/v1"
)

// MockMetricsCollector implements interfaces.MetricsCollector for testing purposes
type MockMetricsCollector struct{}

func (m *MockMetricsCollector) ValidateMetricsAvailability(
	ctx context.Context,
	modelName string,
	namespace string,
) interfaces.MetricsValidationResult {
	return interfaces.MetricsValidationResult{
		Available: true,
		Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
		Message:   "Mock metrics available",
	}
}

func (m *MockMetricsCollector) AddMetricsToOptStatus(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
) (interfaces.OptimizerMetrics, error) {
	// Return fixed mock metrics used for feature construction
	return interfaces.OptimizerMetrics{
		ArrivalRate:     100.0,
		AvgInputTokens:  128.0,
		AvgOutputTokens: 128.0,
		TTFTSeconds:     0.1,  // 100ms
		ITLSeconds:      0.02, // 20ms
	}, nil
}

func (m *MockMetricsCollector) CollectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	metrics := []interfaces.ReplicaMetrics{}
	for _, deploy := range deployments {
		if *deploy.Spec.Replicas > 0 {
			// Create metrics for 'Replicas' count
			count := int(*deploy.Spec.Replicas)
			for i := 0; i < count; i++ {
				metrics = append(metrics, interfaces.ReplicaMetrics{
					PodName:   fmt.Sprintf("%s-pod-%d", deploy.Name, i),
					ModelID:   modelID,
					Namespace: namespace,
					// Mock Latency Data matching AddMetricsToOptStatus
					ArrivalRate:  100.0,
					AvgInputLen:  128.0,
					AvgOutputLen: 128.0,
					AvgITL:       20.0,
					AvgTTFT:      100.0,
				})
			}
		}
	}
	// Fallback if no deps
	if len(metrics) == 0 {
		metrics = append(metrics, interfaces.ReplicaMetrics{
			PodName:      "mock-pod-0",
			ModelID:      modelID,
			Namespace:    namespace,
			ArrivalRate:  100.0,
			AvgInputLen:  128.0,
			AvgOutputLen: 128.0,
			AvgITL:       20.0,
			AvgTTFT:      100.0,
		})
	}
	return metrics, nil
}
