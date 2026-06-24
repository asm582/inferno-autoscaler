# Saturation V1 HPA — Setup Guide

This guide shows how to replicate the Saturation V1 autoscaling logic from the Workload Variant Autoscaler (WVA) using a plain Kubernetes `HorizontalPodAutoscaler` object and a Prometheus Adapter. Use this when you want reactive, metric-driven scaling based on KV cache utilization and request queue depth — without deploying the full WVA controller.

## When to use this approach

| Situation | Recommendation |
|---|---|
| Single Deployment per model, no variant cost-optimization needed | **This guide** |
| Multiple GPU variants per model, cost-aware placement | Use WVA Saturation V1 |
| You need scale-to-zero | Native HPA supports scale to zero (Kubernetes 1.32+); use WVA or KEDA on older clusters |

---

## How the mapping works

Saturation V1 scales up when the average **spare** capacity across replicas drops below a trigger:

```
scale_up if:
  avg_spare_kv    < KvSpareTrigger    (default 0.10)  →  avg KV usage > 0.70
  OR
  avg_spare_queue < QueueSpareTrigger (default 3.0)   →  avg queue    > 2.0
```

An HPA with `averageValue` targets achieves the same effect:

```
desiredReplicas = ceil(currentReplicas × currentAvg / targetAvg)
```

Setting `targetAvg = 0.70` for KV cache and `targetAvg = 2` for queue length reproduces the V1 scale-up trigger exactly. HPA picks the **maximum** desired replica count across all configured metrics, which mirrors V1's OR logic.

### Default threshold mapping

| V1 parameter | Default | HPA `averageValue` target | Formula |
|---|---|---|---|
| `KvCacheThreshold` | 0.80 | — | saturation boundary |
| `KvSpareTrigger` | 0.10 | `700m` (= 0.70) | `KvCacheThreshold − KvSpareTrigger` |
| `QueueLengthThreshold` | 5.0 | — | saturation boundary |
| `QueueSpareTrigger` | 3.0 | `2` | `QueueLengthThreshold − QueueSpareTrigger` |

---

## Prerequisites

Before applying any YAML, verify the following are in place.

### 1. vLLM pods are exposing Prometheus metrics

vLLM exposes metrics on port `8080` at `/metrics` by default. Confirm the two metrics used by this guide are present:

```bash
# Port-forward to any vLLM pod and check
kubectl port-forward pod/<vllm-pod-name> 8080:8080 -n <namespace>
curl -s http://localhost:8080/metrics | grep -E 'vllm:kv_cache_usage_perc|vllm:num_requests_waiting'
```

Expected output (values will vary):
```
vllm:kv_cache_usage_perc{model_name="your-model",...} 0.43
vllm:num_requests_waiting{model_name="your-model",...} 1
```

### 2. Prometheus is scraping vLLM pods

Verify Prometheus has data for the metrics. Open the Prometheus UI or run:

```bash
kubectl port-forward svc/prometheus-operated 9090:9090 -n monitoring
# Then open http://localhost:9090 and query:
# vllm:kv_cache_usage_perc
# vllm:num_requests_waiting
```

If no data appears, create a `PodMonitor` (assuming kube-prometheus-stack is installed):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: vllm-metrics
  namespace: <namespace>
spec:
  selector:
    matchLabels:
      app: vllm-inference    # replace with your pod label
  podMetricsEndpoints:
    - port: metrics           # replace with the port name on your pod
      path: /metrics
```

```bash
kubectl apply -f vllm-podmonitor.yaml
```

### 3. Prometheus Adapter is installed

```bash
kubectl get deployment prometheus-adapter -n monitoring
```

If not installed, use Helm:

> **Note:** prometheus-adapter is [deprecated](https://github.com/kubernetes-sigs/prometheus-adapter/issues/701).
> Consider a maintained alternative (e.g. the [custom-metrics-stackdriver-adapter](https://github.com/GoogleCloudPlatform/k8s-stackdriver) or a vendor-provided adapter) for production use.

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install prometheus-adapter prometheus-community/prometheus-adapter \
  --namespace monitoring \
  --set prometheus.url=http://prometheus-operated.monitoring.svc
  # --set prometheus.port=9090  # default; omit unless Prometheus uses a non-standard port
```

---

## Step 1 — Configure Prometheus Adapter rules

The Adapter needs rules that expose the two vLLM PromQL queries as named Kubernetes external metrics. The queries mirror V1's `max_over_time` window to catch saturation spikes between scrapes.

Create or edit the Adapter's ConfigMap. The default ConfigMap name is `prometheus-adapter` in the `monitoring` namespace:

```bash
kubectl edit cm prometheus-adapter -n monitoring
```

Add the following under the `rules:` key in the `config.yaml` data field:

```yaml
rules:
  # KV cache utilization (0.0–1.0), peak over last minute per pod
  - seriesQuery: 'vllm:kv_cache_usage_perc{namespace!="",pod!=""}'
    resources:
      overrides:
        namespace: {resource: "namespace"}
        pod:       {resource: "pod"}
    name:
      matches: "vllm:kv_cache_usage_perc"
      as: "vllm_kv_cache_usage_perc"
    metricsQuery: |
      max by (<<.GroupBy>>, model_name) (
        max_over_time(vllm:kv_cache_usage_perc{<<.LabelMatchers>>}[1m])
      )

  # Waiting request queue length, peak over last minute per pod
  - seriesQuery: 'vllm:num_requests_waiting{namespace!="",pod!=""}'
    resources:
      overrides:
        namespace: {resource: "namespace"}
        pod:       {resource: "pod"}
    name:
      matches: "vllm:num_requests_waiting"
      as: "vllm_num_requests_waiting"
    metricsQuery: |
      max by (<<.GroupBy>>, model_name) (
        max_over_time(vllm:num_requests_waiting{<<.LabelMatchers>>}[1m])
      )
```

After saving, restart the Adapter to pick up the changes:

```bash
kubectl rollout restart deployment/prometheus-adapter -n monitoring
```

Verify the metrics are registered (wait ~30 seconds for the Adapter to start):

```bash
kubectl get --raw '/apis/external.metrics.k8s.io/v1beta1' | jq '.resources[].name'
# Should include "vllm_kv_cache_usage_perc" and "vllm_num_requests_waiting"
```

---

## Step 2 — Apply the HPA

Save the following as `vllm-saturation-hpa.yaml`. Replace the four placeholder values marked with comments.

```yaml
# Saturation V1 HPA
# Scale-up triggers (mirroring V1 defaults):
#   KV cache  → target averageValue = KvCacheThreshold(0.80) − KvSpareTrigger(0.10) = 0.70
#   Queue     → target averageValue = QueueLengthThreshold(5.0) − QueueSpareTrigger(3.0) = 2.0
# HPA picks the MAX desired replicas across both metrics — equivalent to V1's OR logic.
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-saturation-hpa
  namespace: <namespace>                      # replace
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: <vllm-deployment-name>              # replace

  # minReplicas: 1                            # Kubernetes default; omit to inherit
  maxReplicas: 10                             # replace with your upper bound

  metrics:

    # KV cache utilization (0.0–1.0)
    # Fires when per-pod average exceeds 0.70 (= 80% threshold − 10% spare trigger)
    - type: External
      external:
        metric:
          name: vllm_kv_cache_usage_perc
          selector:
            matchLabels:
              model_name: "<your-model-name>" # replace — must match vLLM's model_name label
        target:
          type: AverageValue
          averageValue: "700m"                # 0.700 in Kubernetes quantity notation

    # Waiting request queue length
    # Fires when per-pod average exceeds 2.0 (= 5 threshold − 3 spare trigger)
    - type: External
      external:
        metric:
          name: vllm_num_requests_waiting
          selector:
            matchLabels:
              model_name: "<your-model-name>" # replace — must match vLLM's model_name label
        target:
          type: AverageValue
          averageValue: "2"                   # 2 waiting requests per replica

  behavior:
    scaleUp:
      # React immediately — V1 has no scale-up cooldown window
      # stabilizationWindowSeconds: 0  # Kubernetes default for scaleUp; omit to inherit
      policies:
        # Add one pod at a time. periodSeconds must be >= pod startup time so that
        # the HPA waits for the new replica to be ready before firing again — this
        # approximates V1's readiness-based transition blocking. For Qwen models on
        # H100 hardware, pod startup is typically 60–100 s; 180 s gives safe headroom.
        - type: Pods
          value: 1
          periodSeconds: 180
      # selectPolicy: Max  # Kubernetes default for scaleUp; omit to inherit

    scaleDown:
      # Hold 5 minutes before scaling down — approximates V1's N/(N-1) safety simulation
      # stabilizationWindowSeconds: 300  # Kubernetes default for scaleDown; omit to inherit
      policies:
        # Remove one pod at a time, matching V1's single-step scale-down rule
        - type: Pods
          value: 1
          periodSeconds: 300
      selectPolicy: Min
```

Apply it:

```bash
kubectl apply -f vllm-saturation-hpa.yaml
```

---

## Step 3 — Verify the HPA is working

Check the HPA status:

```bash
kubectl get hpa vllm-saturation-hpa -n <namespace>
```

Expected output when metrics are flowing:
```
NAME                   REFERENCE                         TARGETS                    MINPODS   MAXPODS   REPLICAS
vllm-saturation-hpa   Deployment/vllm-inference          450m/700m, 0/2             1         10        2
```

If `TARGETS` shows `<unknown>`, the Adapter is not yet serving the metrics — see [Troubleshooting](#troubleshooting).

For a full event log:
```bash
kubectl describe hpa vllm-saturation-hpa -n <namespace>
```

---

## Tuning thresholds

To match different saturation boundaries, recalculate the `averageValue` targets using the same formula:

```
KV averageValue   = kvCacheThreshold - kvSpareTrigger
Queue averageValue = queueLengthThreshold - queueSpareTrigger
```

### Common preset configurations

| Profile | `kvCacheThreshold` | `kvSpareTrigger` | KV `averageValue` | `queueLengthThreshold` | `queueSpareTrigger` | Queue `averageValue` |
|---|---|---|---|---|---|---|
| **Default (V1 baseline)** | 0.80 | 0.10 | `700m` | 5 | 3 | `2` |
| **Aggressive** (high GPU utilization) | 0.90 | 0.05 | `850m` | 15 | 5 | `10` |
| **Conservative** (low-latency SLO) | 0.70 | 0.15 | `550m` | 3 | 2 | `1` |

Edit the HPA in place to update thresholds:

```bash
kubectl edit hpa vllm-saturation-hpa -n <namespace>
```

Changes take effect immediately — no pod restart needed.

---

## P/D disaggregated deployments

> **TODO**: Document HPA setup for prefill/decode disaggregated deployments (one HPA per Deployment).

---

## Limitations vs WVA Saturation V1

| V1 capability | HPA equivalent | Gap |
|---|---|---|
| Scale-up OR logic (KV or queue) | HPA max across metrics | None — exact match |
| +1 / −1 pod at a time | `policies: Pods value: 1` | None — exact match |
| Transition blocking (wait for pods to be ready) | `periodSeconds` on the scale-up policy | Approximate — set `periodSeconds` ≥ pod startup time; V1 checks actual pod readiness, HPA uses a fixed timer |
| Scale-down safety simulation (N/N−1 load redistribution) | `stabilizationWindowSeconds: 300` | Approximate — time-based, not load-simulation-based |
| Scale-to-zero | Requires KEDA | Not supported by native HPA |
| Cascade scale-up prevention (skip variants with pending pods) | HPA readiness gates on Deployment | Approximate |

---

## Benchmarking

A ready-to-run benchmark scenario is provided in `hack/benchmark/scenarios/saturation-v1-hpa.yaml`. It installs the WVA stack (for prometheus-adapter and monitoring), then swaps the WVA-managed HPA with the Saturation V1 HPA described in this guide.

### Configurable parameters

| Makefile variable | Default | Description |
|---|---|---|
| `HPA_KV_CACHE_TARGET` | `700m` | KV cache `averageValue` target (0.70) |
| `HPA_QUEUE_TARGET` | `2` | Queue length `averageValue` target |
| `HPA_MIN_REPLICAS` | `1` | HPA minimum replica count |
| `HPA_MAX_REPLICAS` | `10` | HPA maximum replica count |
| `HPA_SCALE_UP_PERIOD_SEC` | `180` | `periodSeconds` for scale-up policy |
| `HPA_SCALE_DOWN_PERIOD_SEC` | `300` | `periodSeconds` for scale-down policy |
| `HPA_BENCHMARK_HARNESS` | `inference-perf` | Default harness (overridable via `BENCHMARK_HARNESS`) |
| `HPA_BENCHMARK_WORKLOAD` | `shared_prefix_synthetic.yaml` | Default workload (overridable via `BENCHMARK_WORKLOAD`) |

### Stand up the environment

```bash
make benchmark-standup-hpa \
  BENCHMARK_NAMESPACE=<namespace> \
  BENCHMARK_MODEL_ID=<model-id>
```

This renders the scenario, deploys the stack, then runs `hack/benchmark-swap-hpa.py` to replace the WVA HPA with the Saturation V1 HPA using the configured targets and scale periods.

### Run a benchmark

```bash
# Using the default harness and workload
make benchmark-run-hpa \
  BENCHMARK_NAMESPACE=<namespace> \
  BENCHMARK_MODEL_ID=<model-id>

# Override harness and workload
make benchmark-run-hpa \
  BENCHMARK_NAMESPACE=<namespace> \
  BENCHMARK_MODEL_ID=<model-id> \
  BENCHMARK_HARNESS=guidellm \
  BENCHMARK_WORKLOAD=prefill_heavy.yaml
```

### Tuning scale-up period

The `HPA_SCALE_UP_PERIOD_SEC` default of 180 s is calibrated to Qwen-class models on H100 hardware where pod startup takes 60–100 s. If your model starts faster or slower, adjust accordingly:

```bash
# Faster-starting model (e.g., pod ready in ~30 s)
make benchmark-standup-hpa \
  BENCHMARK_NAMESPACE=<namespace> \
  BENCHMARK_MODEL_ID=<model-id> \
  HPA_SCALE_UP_PERIOD_SEC=60

# Slower-starting model (e.g., pod ready in ~120 s)
make benchmark-standup-hpa \
  BENCHMARK_NAMESPACE=<namespace> \
  BENCHMARK_MODEL_ID=<model-id> \
  HPA_SCALE_UP_PERIOD_SEC=240
```
