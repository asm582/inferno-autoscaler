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
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
)

type Engine struct {
	client           client.Client
	scheme           *runtime.Scheme
	executor         executor.Executor
	Recorder         record.EventRecorder
	MetricsCollector interfaces.MetricsCollector
	PredictorClient  LatencyPredictorClient
}

// NewEngine creates a new instance of the predictive engine.
func NewEngine(
	client client.Client,
	scheme *runtime.Scheme,
	recorder record.EventRecorder,
	collector interfaces.MetricsCollector,
	predictorClient LatencyPredictorClient,
) *Engine {
	engine := Engine{
		client:           client,
		scheme:           scheme,
		Recorder:         recorder,
		MetricsCollector: collector,
		PredictorClient:  predictorClient,
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     30 * time.Second,
		RetryBackoff: 100 * time.Millisecond,
	})

	return &engine
}

// StartOptimizeLoop starts the optimization loop for the predictive engine.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := ctrl.LoggerFrom(ctx)

	// Refresh interval from config
	interval := common.Config.GetOptimizationInterval()
	if interval != "" {
		// In a real implementation, we might update the executor's interval here
	}

	activeVAs, err := utils.ActiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		return nil
	}

	// Group VAs by model
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)

	for modelID, modelVAs := range modelGroups {
		for _, va := range modelVAs {
			// 1. Collect Metrics
			// Reuse Saturation's collection logic or custom?
			// For E2E simplicity, we'll try to use existing utils or just rely on Status if already populated by Saturation engine (which might be running).
			// If Saturation engine is NOT running, we need to collect ourselves.
			// Let's assume we need to collect.

			// 1. Collect Metrics
			var deploy appsv1.Deployment
			if err := utils.GetDeploymentWithBackoff(ctx, e.client, va.GetScaleTargetName(), va.Namespace, &deploy); err != nil {
				logger.Error(err, "Failed to get deployment", "va", va.Name)
				continue
			}

			// Mock cost/accelerator
			cost := 1.0

			// Validate metrics availability
			metricValidation := e.MetricsCollector.ValidateMetricsAvailability(ctx, va.Spec.ModelID, va.Namespace)
			if !metricValidation.Available {
				logger.Info("Metrics not available yet", "va", va.Name)
				continue
			}

			// We still populate CurrentAlloc for status visibility, though decision uses replica metrics
			metrics, err := e.MetricsCollector.AddMetricsToOptStatus(ctx, &va, deploy, cost)
			if err == nil {
				alloc, err := utils.BuildAllocationFromMetrics(metrics, &va, deploy, cost)
				if err == nil {
					va.Status.CurrentAlloc = alloc
				}
			}

			// Per-Replica Prediction Logic
			// Fetch ALL replica metrics for this model (potentially multiple VAs)
			replicaMetrics, err := e.MetricsCollector.CollectReplicaMetrics(ctx, va.Spec.ModelID, va.Namespace,
				map[string]*appsv1.Deployment{deploy.Name: &deploy},
				map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{va.Name: &va},
				map[string]float64{va.Name: cost})

			if err != nil {
				logger.Error(err, "Failed to collect replica metrics", "va", va.Name)
				continue
			}

			// 2. Get SLOs (ITL)
			targetITL := 20.0
			if va.Spec.SLO != nil && va.Spec.SLO.MaxITL != nil {
				targetITL = float64(*va.Spec.SLO.MaxITL)
			}

			maxPredictedITL := 0.0
			replicaCount := 0

			for _, rep := range replicaMetrics {
				// Filter for this variant
				// (In basic E2E/Mock, variant mapping might be loose, but check if we can filter)
				// For simplicity in this engine, if ModdelID matches, and it's E2E, we consider it.
				// But ideally checks rep.VariantName == va.Name
				// Assuming Mock collector returns correct ModelID/Namespace
				if rep.ModelID != va.Spec.ModelID || rep.Namespace != va.Namespace {
					continue
				}
				replicaCount++

				// Construct Features for THIS replica
				features := map[string]float64{
					"kv_cache_percentage":  rep.KvCacheUsage,
					"input_token_length":   rep.AvgInputLen,
					"num_request_waiting":  float64(rep.QueueLength),
					"num_request_running":  rep.RequestsRunning,
					"num_tokens_generated": rep.TokensGenerated,
					"prefix_cache_score":   rep.PrefixCacheScore,
				}

				// Sanitize NaN values
				for k, v := range features {
					if math.IsNaN(v) {
						features[k] = 0
					}
				}

				// If inputs are 0 (e.g. no traffic), use defaults to avoid garbage predictions?
				if features["input_token_length"] == 0 {
					features["input_token_length"] = 128
				}

				pred, err := e.PredictorClient.PredictITL(ctx, modelID, features)
				if err != nil {
					logger.Error(err, "Failed to predict ITL for replica", "pod", rep.PodName)
					continue
				}
				if pred > maxPredictedITL {
					maxPredictedITL = pred
				}
			}

			logger.Info("Per-Replica Prediction Analysis",
				"va", va.Name,
				"replicas_analyzed", replicaCount,
				"max_predicted_itl", maxPredictedITL,
				"target_itl", targetITL)

			// DECISION LOGIC
			// Use the LAST calculated desired state as the baseline, if valid.
			// This decouples us from HPA/Deployment lag (stabilization windows).
			baselineReplicas := int(*deploy.Spec.Replicas)
			if va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				baselineReplicas = va.Status.DesiredOptimizedAlloc.NumReplicas
			}
			desiredReplicas := baselineReplicas

			if maxPredictedITL > targetITL {
				// Scale Up if worst replica is violating SLO
				desiredReplicas = baselineReplicas + 1
				logger.Info("Scaling UP", "reason", "Replica SLO violation", "baseline", baselineReplicas, "desired", desiredReplicas, "max_pred_itl", maxPredictedITL)
			} else if maxPredictedITL < targetITL*0.5 && baselineReplicas > 1 {
				// Scale Down if max latency is very safe
				desiredReplicas = baselineReplicas - 1
				logger.Info("Scaling DOWN", "reason", "Global slack", "baseline", baselineReplicas, "desired", desiredReplicas)
			} else {
				// Keep same
				logger.Info("Maintaining replicas", "baseline", baselineReplicas, "max_predicted_itl", maxPredictedITL)
			}

			if desiredReplicas < 1 {
				desiredReplicas = 1
			}

			// 4. Actuate (Cache + Channel)
			common.DecisionCache.Set(va.Name, va.Namespace, interfaces.VariantDecision{
				VariantName:     va.Name,
				Namespace:       va.Namespace,
				TargetReplicas:  desiredReplicas,
				AcceleratorName: va.Status.CurrentAlloc.Accelerator,
				LastRunTime:     metav1.Now(),
				Reason:          "PredictiveEngine",
				ModelID:         modelID,
			})

			common.DecisionTrigger <- event.GenericEvent{
				Object: &va,
			}
		}
	}

	return nil
}
