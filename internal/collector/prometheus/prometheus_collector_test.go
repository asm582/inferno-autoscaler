package prometheus

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	internalutils "github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	"github.com/llm-d-incubation/workload-variant-autoscaler/test/utils"
)

var _ = Describe("PrometheusCollector", func() {
	var (
		ctx         context.Context
		mockPromAPI *utils.MockPromAPI
		collector   *PrometheusCollector
		va          *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		deployment  appsv1.Deployment
		modelName   string
		namespace   string
		variantName string
	)

	BeforeEach(func() {
		ctx = ctrl.LoggerInto(context.Background(), logging.NewTestLogger())

		modelName = "test-model"
		namespace = "test-namespace"
		variantName = "test-variant"

		mockPromAPI = &utils.MockPromAPI{
			QueryResults: make(map[string]model.Value),
			QueryErrors:  make(map[string]error),
		}

		// Create collector with cache enabled, but disable background fetching for tests
		testConfig := &config.CacheConfig{
			Enabled:         true,
			TTL:             30 * time.Second,
			CleanupInterval: 1 * time.Minute,
			FetchInterval:   0, // Disable background fetching in tests
		}
		collector = NewPrometheusCollectorWithConfig(mockPromAPI, testConfig)

		// Setup VariantAutoscaling
		va = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{
				Name:      variantName,
				Namespace: namespace,
				Labels: map[string]string{
					"inference.optimization/acceleratorName": "A100",
				},
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ModelID: modelName,
			},
		}

		// Setup Deployment
		replicas := int32(2)
		deployment = appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      variantName,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
			},
		}

		// Setup mock Prometheus responses
		setupMockPrometheusQueries(mockPromAPI, modelName, namespace)
	})

	Describe("AddMetricsToOptStatus with cache", func() {
		It("should cache metrics after first query", func() {
			acceleratorCost := 10.0

			// First call - should query Prometheus
			metrics1, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(alloc1.Accelerator).To(Equal("A100"))

			// Verify cache was populated by calling again (should use cache)
			// We can't directly access private cache field, so we verify behavior
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Should return same data (from cache) - compare without metadata timestamps
			Expect(alloc2.Accelerator).To(Equal(alloc1.Accelerator))
			Expect(alloc2.NumReplicas).To(Equal(alloc1.NumReplicas))
			Expect(alloc2.MaxBatch).To(Equal(alloc1.MaxBatch))
			Expect(alloc2.ITLAverage).To(Equal(alloc1.ITLAverage))
			Expect(alloc2.TTFTAverage).To(Equal(alloc1.TTFTAverage))
			Expect(alloc2.Load).To(Equal(alloc1.Load))
		})

		It("should return cached metrics on second call", func() {
			acceleratorCost := 10.0

			// First call - query Prometheus
			metrics1, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Clear mock to ensure we're using cache
			mockPromAPI.QueryResults = make(map[string]model.Value)
			mockPromAPI.QueryErrors = make(map[string]error)

			// Second call - should use cache
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Compare without metadata timestamps (they differ slightly due to cache retrieval time)
			Expect(alloc2.Accelerator).To(Equal(alloc1.Accelerator))
			Expect(alloc2.NumReplicas).To(Equal(alloc1.NumReplicas))
			Expect(alloc2.MaxBatch).To(Equal(alloc1.MaxBatch))
			Expect(alloc2.ITLAverage).To(Equal(alloc1.ITLAverage))
			Expect(alloc2.TTFTAverage).To(Equal(alloc1.TTFTAverage))
			Expect(alloc2.Load).To(Equal(alloc1.Load))
		})

		It("should query again after cache expiration", func() {
			acceleratorCost := 10.0

			// Create collector with very short TTL using NewPrometheusCollectorWithConfig
			shortTTLConfig := &config.CacheConfig{
				Enabled:         true,
				TTL:             100 * time.Millisecond,
				CleanupInterval: 1 * time.Minute,
			}
			collector = NewPrometheusCollectorWithConfig(mockPromAPI, shortTTLConfig)

			// First call
			metrics1, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Wait for cache expiration
			time.Sleep(150 * time.Millisecond)

			// Setup new mock response
			setupMockPrometheusQueries(mockPromAPI, modelName, namespace)
			// Modify response to verify new query
			arrivalQuery := getArrivalQuery(modelName, namespace)
			mockPromAPI.QueryResults[arrivalQuery] = model.Vector{
				&model.Sample{Value: model.SampleValue(100.0)},
			}

			// Second call - should query again
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Should have new data (different arrival rate)
			Expect(alloc2.Load.ArrivalRate).NotTo(Equal(alloc1.Load.ArrivalRate))
		})

		It("should invalidate cache for variant", func() {
			acceleratorCost := 10.0

			// First call - populate cache
			_, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Populate cache by calling twice (second call uses cache)
			metrics1, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Clear mock to verify cache is used
			mockPromAPI.QueryResults = make(map[string]model.Value)
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(alloc2).To(Equal(alloc1)) // Should use cache

			// Invalidate cache
			collector.InvalidateCacheForVariant(modelName, namespace, variantName)

			// Setup mock again - should query after invalidation
			setupMockPrometheusQueries(mockPromAPI, modelName, namespace)
			metrics3, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc3, err := internalutils.BuildAllocationFromMetrics(metrics3, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Should have queried again (not from cache)
			Expect(alloc3.Accelerator).To(Equal("A100"))
		})
	})

	Describe("Cache invalidation on scaling", func() {
		It("should invalidate cache when variant is invalidated", func() {
			acceleratorCost := 10.0

			// Populate cache
			metrics1, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Verify cache works (second call uses cache)
			mockPromAPI.QueryResults = make(map[string]model.Value)
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Compare without metadata timestamps (they differ slightly due to cache retrieval time)
			Expect(alloc2.Accelerator).To(Equal(alloc1.Accelerator))
			Expect(alloc2.NumReplicas).To(Equal(alloc1.NumReplicas))
			Expect(alloc2.Load).To(Equal(alloc1.Load))

			// Invalidate
			collector.InvalidateCacheForVariant(modelName, namespace, variantName)

			// Setup mock again - should query after invalidation
			setupMockPrometheusQueries(mockPromAPI, modelName, namespace)
			metrics3, err := collector.AddMetricsToOptStatus(ctx, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc3, err := internalutils.BuildAllocationFromMetrics(metrics3, va, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(alloc3.Accelerator).To(Equal("A100"))
		})

		It("should invalidate all model cache entries", func() {
			acceleratorCost := 10.0

			// Populate cache for multiple variants
			va1 := va.DeepCopy()
			va1.Name = "variant1"
			va2 := va.DeepCopy()
			va2.Name = "variant2"

			metrics1, err := collector.AddMetricsToOptStatus(ctx, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1, err := internalutils.BuildAllocationFromMetrics(metrics1, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			metrics2, err := collector.AddMetricsToOptStatus(ctx, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2, err := internalutils.BuildAllocationFromMetrics(metrics2, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())

			// Verify cache works
			mockPromAPI.QueryResults = make(map[string]model.Value)
			metrics1Cached, err := collector.AddMetricsToOptStatus(ctx, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1Cached, err := internalutils.BuildAllocationFromMetrics(metrics1Cached, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Compare without metadata timestamps (they differ slightly due to cache retrieval time)
			Expect(alloc1Cached.Accelerator).To(Equal(alloc1.Accelerator))
			Expect(alloc1Cached.NumReplicas).To(Equal(alloc1.NumReplicas))
			Expect(alloc1Cached.Load).To(Equal(alloc1.Load))
			metrics2Cached, err := collector.AddMetricsToOptStatus(ctx, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2Cached, err := internalutils.BuildAllocationFromMetrics(metrics2Cached, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			Expect(alloc2Cached.Accelerator).To(Equal(alloc2.Accelerator))
			Expect(alloc2Cached.NumReplicas).To(Equal(alloc2.NumReplicas))
			Expect(alloc2Cached.Load).To(Equal(alloc2.Load))

			// Invalidate for entire model
			collector.InvalidateCacheForModel(modelName, namespace)

			// Setup mock again - should query after invalidation
			setupMockPrometheusQueries(mockPromAPI, modelName, namespace)
			metrics1After, err := collector.AddMetricsToOptStatus(ctx, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc1After, err := internalutils.BuildAllocationFromMetrics(metrics1After, va1, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			metrics2After, err := collector.AddMetricsToOptStatus(ctx, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			alloc2After, err := internalutils.BuildAllocationFromMetrics(metrics2After, va2, deployment, acceleratorCost)
			Expect(err).NotTo(HaveOccurred())
			// Should have queried again
			Expect(alloc1After.Accelerator).To(Equal("A100"))
			Expect(alloc2After.Accelerator).To(Equal("A100"))
		})
	})
})

// Helper functions
func setupMockPrometheusQueries(mockProm *utils.MockPromAPI, modelName, namespace string) {
	arrivalQuery := getArrivalQuery(modelName, namespace)
	avgPromptToksQuery := getAvgPromptToksQuery(modelName, namespace)
	avgDecToksQuery := getAvgDecToksQuery(modelName, namespace)
	ttftQuery := getTTFTQuery(modelName, namespace)
	itlQuery := getITLQuery(modelName, namespace)

	mockProm.QueryResults[arrivalQuery] = model.Vector{
		&model.Sample{Value: model.SampleValue(10.0)},
	}
	mockProm.QueryResults[avgPromptToksQuery] = model.Vector{
		&model.Sample{Value: model.SampleValue(100.0)},
	}
	mockProm.QueryResults[avgDecToksQuery] = model.Vector{
		&model.Sample{Value: model.SampleValue(50.0)},
	}
	mockProm.QueryResults[ttftQuery] = model.Vector{
		&model.Sample{Value: model.SampleValue(0.1)},
	}
	mockProm.QueryResults[itlQuery] = model.Vector{
		&model.Sample{Value: model.SampleValue(0.05)},
	}
}

func getArrivalQuery(modelName, namespace string) string {
	return `sum(rate(` + constants.VLLMRequestSuccessTotal + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))`
}

func getAvgPromptToksQuery(modelName, namespace string) string {
	return `sum(rate(` + constants.VLLMRequestPromptTokensSum + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))/sum(rate(` + constants.VLLMRequestPromptTokensCount + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))`
}

func getAvgDecToksQuery(modelName, namespace string) string {
	return `sum(rate(` + constants.VLLMRequestGenerationTokensSum + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))/sum(rate(` + constants.VLLMRequestGenerationTokensCount + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))`
}

func getTTFTQuery(modelName, namespace string) string {
	return `sum(rate(` + constants.VLLMTimeToFirstTokenSecondsSum + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))/sum(rate(` + constants.VLLMTimeToFirstTokenSecondsCount + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))`
}

func getITLQuery(modelName, namespace string) string {
	return `sum(rate(` + constants.VLLMTimePerOutputTokenSecondsSum + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))/sum(rate(` + constants.VLLMTimePerOutputTokenSecondsCount + `{` + constants.LabelModelName + `="` + modelName + `",` + constants.LabelNamespace + `="` + namespace + `"}[1m]))`
}
