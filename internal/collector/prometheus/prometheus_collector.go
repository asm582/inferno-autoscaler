package prometheus

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/cache"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/config"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logging"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/engines/executor"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PrometheusCollector implements MetricsCollector interface for Prometheus backend
type PrometheusCollector struct {
	promAPI promv1.API
	cache   cache.MetricsCache // Internal cache for metrics

	// k8sClient is accessed by background goroutines, so we use atomic.Value for thread-safe access
	// We use a mutex to protect writes (SetK8sClient)
	k8sClientMu sync.RWMutex
	k8sClient   client.Client

	// Background fetching
	fetchInterval       time.Duration
	freshnessThresholds config.FreshnessThresholds
	fetchExecutor       executor.Executor // Polling executor for background metric fetching
	trackedVAs          sync.Map          // map[string]*TrackedVA - tracks VAs for background fetching
	trackedModels       sync.Map          // map[string]*TrackedModel - tracks models for replica metrics background fetching
}

// NewPrometheusCollector creates a new Prometheus metrics collector with default cache config
// Deprecated: Use NewPrometheusCollectorWithConfig instead
func NewPrometheusCollector(promAPI promv1.API) *PrometheusCollector {
	return NewPrometheusCollectorWithConfig(promAPI, nil)
}

// NewPrometheusCollectorWithConfig creates a new Prometheus metrics collector
// If cacheConfig is nil, uses default cache settings
func NewPrometheusCollectorWithConfig(promAPI promv1.API, cacheConfig *config.CacheConfig) *PrometheusCollector {

	// Use provided config or defaults
	cfg := getDefaultCacheConfig()
	if cacheConfig != nil {
		cfg = cacheConfig
	}

	var metricsCache cache.MetricsCache

	if cfg.Enabled {
		metricsCache = cache.NewMemoryCache(cfg.TTL, cfg.CleanupInterval)
		ctrl.Log.Info("Metrics cache enabled", "TTL", cfg.TTL, "cleanupInterval", cfg.CleanupInterval)
	} else {
		// Use a no-op cache implementation when disabled
		metricsCache = &cache.NoOpCache{}
		ctrl.Log.Info("Metrics cache disabled")
	}

	pc := &PrometheusCollector{
		promAPI:             promAPI,
		k8sClient:           nil, // Will be set when available
		cache:               metricsCache,
		fetchInterval:       cfg.FetchInterval,
		freshnessThresholds: cfg.FreshnessThresholds,
	}

	// Initialize background fetching executor if fetch interval is configured
	if cfg.FetchInterval > 0 {
		pc.fetchExecutor = executor.NewPollingExecutor(executor.PollingConfig{
			Config: executor.Config{
				OptimizeFunc: func(ctx context.Context) error {
					// Fetch both allocation metrics (per-VA) and replica metrics (per-model)
					pc.fetchTrackedVAs(ctx)
					pc.fetchTrackedModels(ctx)
					return nil // Fire-and-forget, errors are logged per-VA/per-model
				},
			},
			Interval:     cfg.FetchInterval,
			RetryBackoff: 500 * time.Millisecond, // Initial backoff for retries (doubles, capped at 4s)
		})
		ctrl.Log.Info("Initialized background fetching executor", "interval", cfg.FetchInterval)
	}

	return pc
}

// SetK8sClient sets the Kubernetes client for pod ownership lookups
// This is thread-safe and can be called concurrently with background fetching
func (pc *PrometheusCollector) SetK8sClient(k8sClient client.Client) {
	pc.k8sClientMu.Lock()
	defer pc.k8sClientMu.Unlock()
	pc.k8sClient = k8sClient
}

// getK8sClient returns the Kubernetes client in a thread-safe manner
func (pc *PrometheusCollector) getK8sClient() client.Client {
	pc.k8sClientMu.RLock()
	defer pc.k8sClientMu.RUnlock()
	return pc.k8sClient
}

// fetchOptimizerMetricsCore contains the core logic for fetching raw optimizer metrics from Prometheus
// This is extracted to be reusable by both on-demand and background fetching
// Returns raw metrics that the controller will use to assemble the Allocation struct
func (pc *PrometheusCollector) fetchOptimizerMetricsCore(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
) (interfaces.OptimizerMetrics, error) {
	deployNamespace := deployment.Namespace
	modelName := va.Spec.ModelID

	// TODO: These queries are optimizer-specific and should be registered by the optimizer.
	// The best place to do this is somewhere under engines/model. This is tracked as a future work item.
	// --- 1. Define Queries ---

	// Metric 1: Arrival rate (requests per minute)
	arrivalQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 2: Average prompt length (Input Tokens)
	avgPromptToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestPromptTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestPromptTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 3: Average decode length (Output Tokens)
	avgDecToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestGenerationTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestGenerationTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 4: Average TTFT (Time To First Token) - Prometheus returns seconds, converted to msec
	ttftQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 5: Average ITL (Inter-Token Latency) - Prometheus returns seconds, converted to msec
	itlQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimePerOutputTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimePerOutputTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// --- 2. Execute Queries ---

	arrivalVal, err := pc.queryAndExtractMetric(ctx, arrivalQuery, "ArrivalRate")
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}
	arrivalVal *= 60 // convert from req/sec to req/min

	avgInputTokens, err := pc.queryAndExtractMetric(ctx, avgPromptToksQuery, "AvgInputTokens")
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}

	avgOutputTokens, err := pc.queryAndExtractMetric(ctx, avgDecToksQuery, "AvgOutputTokens")
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}

	ttftSeconds, err := pc.queryAndExtractMetric(ctx, ttftQuery, "TTFTAverageTime")
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}
	// Keep in seconds - controller will convert to milliseconds

	itlSeconds, err := pc.queryAndExtractMetric(ctx, itlQuery, "ITLAverage")
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}
	// Keep in seconds - controller will convert to milliseconds

	// Return raw metrics - controller will assemble Allocation struct
	metrics := interfaces.OptimizerMetrics{
		ArrivalRate:     arrivalVal, // requests per minute
		AvgInputTokens:  avgInputTokens,
		AvgOutputTokens: avgOutputTokens,
		TTFTSeconds:     ttftSeconds, // seconds (will be converted to ms by controller)
		ITLSeconds:      itlSeconds,  // seconds (will be converted to ms by controller)
	}

	return metrics, nil
}

// ValidateMetricsAvailability implements MetricsCollector interface
func (pc *PrometheusCollector) ValidateMetricsAvailability(
	ctx context.Context,
	modelName string,
	namespace string,
) interfaces.MetricsValidationResult {
	logger := ctrl.LoggerFrom(ctx)

	// Query for basic vLLM metric to validate scraping is working
	// Try with namespace label first (real vLLM), fall back to just model_name (vllme emulator)
	testQuery := fmt.Sprintf(`%s{model_name="%s",namespace="%s"}`, constants.VLLMNumRequestRunning, modelName, namespace)
	logger.Info("Validating metrics availability", "query", testQuery)

	val, _, err := utils.QueryPrometheusWithBackoff(ctx, pc.promAPI, testQuery)
	if err != nil {
		logger.Error(err, "Error querying Prometheus for metrics validation",
			"model", modelName, "namespace", namespace)
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
		}
	}

	// Check if we got any results
	if val.Type() != model.ValVector {
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
			Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check ServiceMonitor configuration and ensure vLLM pods are exposing /metrics endpoint", modelName, namespace),
		}
	}

	vec, ok := val.(model.Vector)
	if !ok {
		// Type mismatch - should not happen but handle gracefully
		return interfaces.MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("prometheus query returned unexpected type (expected Vector, got %s)", val.Type().String()),
		}
	}
	// If no results with namespace label, try without it (for vllme emulator compatibility)
	if len(vec) == 0 {
		testQueryFallback := fmt.Sprintf(`%s{model_name="%s"}`, constants.VLLMNumRequestRunning, modelName)
		val, _, err = utils.QueryPrometheusWithBackoff(ctx, pc.promAPI, testQueryFallback)
		if err != nil {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
			}
		}

		if val.Type() == model.ValVector {
			if v, ok := val.(model.Vector); ok {
				vec = v
			} else {
				// Type mismatch - should not happen but handle gracefully
				return interfaces.MetricsValidationResult{
					Available: false,
					Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
					Message:   "Unexpected type conversion error for Prometheus query result",
				}
			}
		}

		// If still no results, metrics are truly missing
		if len(vec) == 0 {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
				Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check: (1) ServiceMonitor exists in monitoring namespace, (2) ServiceMonitor selector matches vLLM service labels, (3) vLLM pods are running and exposing /metrics endpoint, (4) Prometheus is scraping the monitoring namespace", modelName, namespace),
			}
		}
	}

	// Check if metrics are stale (older than 5 minutes)
	for _, sample := range vec {
		age := time.Since(sample.Timestamp.Time())
		if age > 5*time.Minute {
			return interfaces.MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsStale,
				Message:   fmt.Sprintf("vLLM metrics for model '%s' are stale (last update: %v ago). ServiceMonitor may not be scraping correctly.", modelName, age),
			}
		}
	}

	return interfaces.MetricsValidationResult{
		Available: true,
		Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
		Message:   "vLLM metrics are available and up-to-date",
	}
}

// AddMetricsToOptStatus implements MetricsCollector interface
func (pc *PrometheusCollector) AddMetricsToOptStatus(
	ctx context.Context,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
) (interfaces.OptimizerMetrics, error) {
	logger := ctrl.LoggerFrom(ctx)
	deployNamespace := deployment.Namespace
	modelName := va.Spec.ModelID
	variantName := va.Name

	// Check cache first (always non-blocking)
	// Construct cache key using PrometheusCollector's format: {modelID}/{namespace}/{variantName}/{metricType}
	cacheKey := cache.CacheKey(fmt.Sprintf("%s/%s/%s/allocation-metrics", modelName, deployNamespace, variantName))
	if cached, found := pc.cache.Get(cacheKey); found {
		age := cached.Age()
		freshnessStatus := pc.freshnessThresholds.DetermineStatus(age)
		logger.V(logging.DEBUG).Info("Cache hit for allocation metrics",
			"model", modelName,
			"variant", variantName,
			"age", age.String(),
			"ageSeconds", age.Seconds(),
			"freshnessStatus", freshnessStatus,
			"collectedAt", cached.CollectedAt.Format(time.RFC3339))
		// Type assert to OptimizerMetrics
		if metrics, ok := cached.Data.(interfaces.OptimizerMetrics); ok {
			// Track VA for background fetching
			pc.TrackVA(va, deployment, acceleratorCostVal)
			return metrics, nil
		}
		// If type assertion fails, continue to query
		logger.Info("Cache entry has wrong type, querying Prometheus", "model", modelName, "variant", variantName)
	}

	logger.Info("Cache miss for allocation metrics, querying Prometheus", "model", modelName, "variant", variantName)

	// Use core fetching logic (extracted for reuse by background worker)
	metrics, err := pc.fetchOptimizerMetricsCore(ctx, va, deployment)
	if err != nil {
		return interfaces.OptimizerMetrics{}, err
	}

	// Store in cache (use default TTL from cache config)
	pc.cache.Set(cacheKey, metrics, 0) // 0 means use cache's default TTL
	collectedAt := time.Now()
	logger.V(logging.DEBUG).Info("Collected and cached allocation metrics",
		"model", modelName,
		"variant", variantName,
		"freshnessStatus", "fresh",
		"collectedAt", collectedAt.Format(time.RFC3339))

	// Track VA for background fetching
	pc.TrackVA(va, deployment, acceleratorCostVal)

	return metrics, nil
}

// CollectReplicaMetrics implements MetricsCollector interface
// This method delegates to the SaturationMetricsCollector logic
func (pc *PrometheusCollector) CollectReplicaMetrics(
	ctx context.Context,
	modelID string,
	namespace string,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	variantCosts map[string]float64,
) ([]interfaces.ReplicaMetrics, error) {
	logger := ctrl.LoggerFrom(ctx)
	// Check cache first (cache key is model-level, not variant-level)
	// Construct cache key using PrometheusCollector's format: {modelID}/{namespace}/{variantName}/{metricType}
	replicaCacheKey := cache.CacheKey(fmt.Sprintf("%s/%s/all/replica-metrics", modelID, namespace))
	if cached, found := pc.cache.Get(replicaCacheKey); found {
		age := cached.Age()
		freshnessStatus := pc.freshnessThresholds.DetermineStatus(age)
		// Type assert to []ReplicaMetrics
		if replicaMetrics, ok := cached.Data.([]interfaces.ReplicaMetrics); ok {
			logger.V(logging.DEBUG).Info("Cache hit for replica metrics",
				"model", modelID,
				"namespace", namespace,
				"age", age.String(),
				"ageSeconds", age.Seconds(),
				"freshnessStatus", freshnessStatus,
				"collectedAt", cached.CollectedAt.Format(time.RFC3339),
				"replicaCount", len(replicaMetrics))
			// Add freshness metadata to each replica metric
			for i := range replicaMetrics {
				replicaMetrics[i].Metadata = &interfaces.ReplicaMetricsMetadata{
					CollectedAt:     cached.CollectedAt,
					Age:             age,
					FreshnessStatus: freshnessStatus,
				}
			}
			return replicaMetrics, nil
		}
		// If type assertion fails, continue to query
		logger.Info("Cache entry has wrong type, querying Prometheus", "model", modelID, "namespace", namespace)
	}

	logger.V(logging.DEBUG).Info("Cache miss for replica metrics, querying Prometheus", "model", modelID, "namespace", namespace)

	// Use the existing SaturationMetricsCollector implementation
	// We'll refactor this to be part of PrometheusCollector later, but for now
	// we maintain compatibility by using the existing struct
	saturationCollector := &SaturationMetricsCollector{
		promAPI:   pc.promAPI,
		k8sClient: pc.getK8sClient(),
	}
	replicaMetrics, err := saturationCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, err
	}

	// Add freshness metadata (fresh, just collected)
	collectedAt := time.Now()
	age := time.Duration(0) // Fresh
	freshnessStatus := "fresh"
	for i := range replicaMetrics {
		replicaMetrics[i].Metadata = &interfaces.ReplicaMetricsMetadata{
			CollectedAt:     collectedAt,
			Age:             age,
			FreshnessStatus: freshnessStatus,
		}
	}

	// Store in cache without metadata (metadata added on retrieval)
	cacheMetrics := make([]interfaces.ReplicaMetrics, len(replicaMetrics))
	for i := range replicaMetrics {
		cacheMetrics[i] = replicaMetrics[i]
		cacheMetrics[i].Metadata = nil // Don't store metadata in cache
	}
	// Reuse the same cache key constructed above for storing
	pc.cache.Set(replicaCacheKey, cacheMetrics, 0) // 0 means use cache's default TTL
	logger.V(logging.DEBUG).Info("Collected and cached replica metrics",
		"model", modelID,
		"namespace", namespace,
		"replicaCount", len(replicaMetrics),
		"freshnessStatus", freshnessStatus,
		"collectedAt", collectedAt.Format(time.RFC3339))

	// Track model for background fetching
	pc.TrackModel(modelID, namespace)

	return replicaMetrics, nil
}
