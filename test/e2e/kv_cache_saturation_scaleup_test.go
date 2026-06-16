package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// kvCacheSaturatedFakeMetrics pins the simulator at 95 % KV cache usage with
// no queue pressure. This is the operating point for the KV saturation scale-up
// suite:
//
//   - kv-cache-usage = 0.95 makes every replica "saturated" under the V1
//     analyzer (0.95 >= kvCacheThreshold=0.80), so nonSaturatedCount=0 and
//     avgSpareKvCapacity=0.
//
//   - 0 < kvSpareTrigger=0.10 → ShouldScaleUp=true on every stable evaluation.
//
//   - Because the flag lives in the Deployment pod template, each replica that
//     the HPA adds inherits kv-cache-usage=0.95 automatically. The V1 engine
//     therefore sees the same saturation signal at 2, 3, … 10 replicas and
//     keeps recommending +1 until the VA's maxReplicas cap is reached.
//
//   - Note: with all replicas KV-saturated, avgSpareQueueLength is also 0
//     (nonSaturatedCount=0 means no spare values are accumulated). Both KV and
//     queue spare triggers fire — this is expected V1 behaviour when the entire
//     fleet is saturated; the root cause driving scale-up here is KV cache.
const kvCacheSaturatedFakeMetrics = `{"kv-cache-usage":0.95,"running-requests":1,"waiting-requests":0}`

// V1 saturation config knobs for the KV-cache saturation scale-up suite.
const (
	kvCacheSatKVCacheThreshold     = 0.80 // 0.95 >= 0.80 → every replica is saturated
	kvCacheSatQueueLengthThreshold = 50   // high ceiling; waiting=0 never saturates via queue
	kvCacheSatKVSpareTrigger       = 0.10 // avgSpareKv=0 < 0.10 → scale-up fires
	kvCacheSatQueueSpareTrigger    = 3    // default-level; fires on all-saturated state (see comment above)

	// V2 fields — unused by V1 path but required by buildSaturationConfigYAMLWithThresholds.
	kvCacheSatScaleUpThreshold  = 0.85
	kvCacheSatScaleDownBoundary = 0.70

	kvCacheSatMaxReplicas = int32(10)
)

var _ = Describe("KV cache saturation scale-up to maxReplicas", Label("full"), Ordered, func() {
	const (
		poolName              = "saturation-kvcache-pool"
		modelSvcName          = "saturation-kvcache-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		vaName                = "saturation-kvcache-va"
		hpaName               = "saturation-kvcache"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		// --fake-metrics is a simulator-only flag; real vLLM rejects it.
		if !cfg.UseSimulator {
			Skip("This suite requires the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = "default"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service with kv-cache-usage=0.95 fake metrics")
		// All pods in this Deployment will permanently report kv-cache-usage=0.95.
		// When the HPA adds replica N, that pod also starts at 0.95 — no separate
		// per-pod configuration is required. The V1 engine sees the same saturation
		// signal regardless of replica count and keeps recommending +1 until maxReplicas.
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, vaName,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", kvCacheSaturatedFakeMetrics},
		)).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for initial replica to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Creating VA with minReplicas=1, maxReplicas=10")
		Expect(fixtures.EnsureVariantAutoscaling(
			ctx, crClient, cfg.LLMDNamespace, vaName,
			modelDecodeDeployment, modelID, cfg.AcceleratorType, 30.0, cfg.ControllerInstance,
			fixtures.WithMaxReplicas(kvCacheSatMaxReplicas),
		)).To(Succeed())

		By("Creating HPA / ScaledObject with maxReplicas=10 so the Deployment actually scales")
		if cfg.ScalerBackend == scalerBackendKeda {
			Expect(fixtures.EnsureScaledObject(
				ctx, crClient, cfg.LLMDNamespace, hpaName, modelDecodeDeployment, vaName,
				1, kvCacheSatMaxReplicas, cfg.MonitoringNS,
			)).To(Succeed())
		} else {
			Expect(fixtures.EnsureHPA(
				ctx, k8sClient, cfg.LLMDNamespace, hpaName, modelDecodeDeployment, vaName,
				1, kvCacheSatMaxReplicas,
			)).To(Succeed())
		}

		By("Installing V1 saturation config (no analyzerName) with kvCacheThreshold=0.80")
		// V1 path: ShouldScaleUp fires when avgSpareKv < kvSpareTrigger.
		// With all replicas at kv=0.95 >= threshold=0.80, nonSaturatedCount=0,
		// avgSpareKv=0, and 0 < 0.10 keeps scale-up active at every stable state.
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"", // empty analyzerName → V1 percentage-based path
			kvCacheSatKVCacheThreshold,
			kvCacheSatQueueLengthThreshold,
			kvCacheSatKVSpareTrigger,
			kvCacheSatQueueSpareTrigger,
			kvCacheSatScaleUpThreshold,
			kvCacheSatScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up KV cache saturation scale-up resources")
		_ = crClient.Delete(ctx, &variantautoscalingv1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
		})
		if cfg.ScalerBackend == scalerBackendKeda {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, hpaName)
		} else {
			_ = fixtures.DeleteHPA(ctx, k8sClient, cfg.LLMDNamespace, hpaName)
		}
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// Verifies that the V1 engine detects saturation from kv-cache-usage=0.95 and
	// produces a positive desired allocation without any external load.
	It("should detect KV cache saturation (0.95) and recommend initial scale-up", func() {
		By("Asserting controller logs show V1 path selected for our model")
		expectAnalyzerPathLog("V1", modelID)

		By("Waiting for VA to receive a positive desired allocation")
		waitForPositiveDesiredAllocation(ctx, cfg.LLMDNamespace, vaName)

		By("Asserting VA recommends more than 1 replica when kv-cache-usage is 0.95")
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			desired := *va.Status.DesiredOptimizedAlloc.NumReplicas
			GinkgoWriter.Printf("  Initial detection: VA desired replicas=%d (kv-cache-usage=0.95)\n", desired)
			g.Expect(desired).To(BeNumerically(">", int32(1)),
				"V1 should recommend more than 1 replica when kv-cache-usage=0.95 exceeds kvCacheThreshold=0.80")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies that each new replica the HPA adds also reports kv-cache-usage=0.95
	// (inherited from the Deployment pod template), so the V1 engine never sees
	// spare KV capacity and continuously recommends adding another replica.
	It("should continuously scale up as each added replica also reports 95% KV cache usage", func() {
		By("Observing VA desired replicas climbing past the midpoint (>=5) as saturation persists")
		var lastSeen int32 = 1
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			current := *va.Status.DesiredOptimizedAlloc.NumReplicas
			if current != lastSeen {
				GinkgoWriter.Printf(
					"  KV saturation scale-up: WVA desired replicas %d → %d "+
						"(each new replica starts at kv-cache-usage=0.95)\n",
					lastSeen, current,
				)
				lastSeen = current
			}
			g.Expect(current).To(BeNumerically(">=", int32(5)),
				"V1 should keep recommending scale-up as every replica reports 0.95 KV usage")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Verifies that WVA caps its recommendation at maxReplicas=10 and that the
	// HPA actually drives the Deployment to 10 replicas.
	//
	// Scale-up sequence (driven entirely by KV cache saturation):
	//   1 → 2 → 3 → … → 10 replicas
	// Each step: V1 sees N replicas all at kv=0.95 → nonSaturatedCount=0 →
	// avgSpareKv=0 < kvSpareTrigger=0.10 → recommends N+1 → HPA scales → repeat.
	// At target=11 the VA's maxReplicas clamp produces 10.
	It("should cap scaling at maxReplicas=10 when all replicas remain at 95% KV cache usage", func() {
		By("Waiting for WVA to recommend exactly 10 replicas (maxReplicas cap)")
		var lastSeen int32 = 0
		Eventually(func(g Gomega) {
			va := &variantautoscalingv1alpha1.VariantAutoscaling{}
			g.Expect(crClient.Get(ctx, client.ObjectKey{
				Namespace: cfg.LLMDNamespace, Name: vaName,
			}, va)).To(Succeed())
			g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).NotTo(BeNil())
			current := *va.Status.DesiredOptimizedAlloc.NumReplicas
			if current != lastSeen {
				GinkgoWriter.Printf(
					"  KV saturation scale-up progress: %d → %d replicas recommended\n",
					lastSeen, current,
				)
				lastSeen = current
			}
			g.Expect(current).To(Equal(kvCacheSatMaxReplicas),
				"WVA should recommend exactly maxReplicas=10 once the cap is reached")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Asserting the Deployment was actually scaled to 10 replicas via HPA")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			actual := *dep.Spec.Replicas
			GinkgoWriter.Printf("  Deployment spec.replicas=%d (target=10)\n", actual)
			g.Expect(actual).To(Equal(kvCacheSatMaxReplicas),
				"HPA should have set Deployment spec.replicas to 10 based on WVA's wva_desired_replicas metric")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})
