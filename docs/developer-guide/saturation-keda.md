# KEDA Saturation Scaling — Setup Guide

This guide shows how to set up reactive, metric-driven autoscaling based on KV cache utilization and request queue depth using a [KEDA](https://keda.sh) `ScaledObject` with Prometheus scalers. KEDA queries Prometheus directly for the EPP pool metrics and auto-creates the underlying `HorizontalPodAutoscaler`. Use this when you want saturation-based scaling without deploying the full WVA controller.

## When to use this approach

| Situation | Recommendation |
|---|---|
| Single Deployment per model, no variant cost-optimization needed | **This guide** |
| Multiple GPU variants per model, cost-aware placement | Use the WVA controller |
| You need scale-to-zero | KEDA supports scale-to-zero via `minReplicaCount: 0` |

---

## How the scaling targets work

### EPP pool metrics

The EPP (inference scheduler) continuously polls every registered pool member and exposes two aggregated metrics at its `:9090/metrics` endpoint:

**`inference_pool_average_kv_cache_utilization`**
The average fraction of KV cache blocks in use across all pods in the pool, expressed as a value between `0.0` (empty) and `1.0` (fully saturated). KV cache holds the key/value tensors for active request contexts — when it fills up, new requests cannot be scheduled and latency spikes. EPP computes this as a simple mean over all registered pool members at each scrape interval.

**`inference_pool_average_queue_size`**
The average number of requests waiting to be scheduled (not yet assigned to a pod) across the pool. A growing queue means the pool cannot keep pace with incoming traffic — either KV cache pressure is blocking new token generation, or all pods are fully loaded. EPP aggregates this from each member's reported waiting-request count.

Both metrics carry a `name` label identifying the pool (matching `InferencePool.metadata.name`) and a `namespace` label added by Prometheus during scrape.

### Scaling logic

Each trigger uses `metricType: AverageValue`. KEDA feeds the query result to the HPA it generates, which computes:

```
desiredReplicas = ceil(queryValue / threshold)
```

The generated HPA picks the **maximum** desired replica count across both triggers — scale-up fires if either KV cache or queue exceeds its threshold (OR logic).

### Default thresholds

| Metric | `threshold` | Meaning |
|---|---|---|
| `inference_pool_average_kv_cache_utilization` | `0.7` | Scale up when pool-avg KV cache exceeds 70% per replica |
| `inference_pool_average_queue_size` | `2` | Scale up when pool-avg queue exceeds 2 requests per replica |

---

## Prerequisites

Before applying any YAML, verify the following are in place.

### 1. EPP is deployed and InferencePool has registered members

```bash
kubectl get inferencepool -n <namespace>
```

Pods must carry the labels matching `InferencePool.spec.selector.matchLabels` (e.g. `llm-d.ai/guide: <pool-name>`) to register as pool members. EPP only computes pool averages when at least one pod is registered.

Verify EPP is emitting metrics:

```bash
kubectl port-forward svc/<epp-service-name> 9090:9090 -n <namespace>
TOKEN=$(kubectl create token <prometheus-sa-name> -n <prometheus-namespace>)
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:9090/metrics \
  | grep -E 'inference_pool_average_kv_cache_utilization|inference_pool_average_queue_size'
```

### 2. Prometheus has read access to EPP metrics

EPP's metrics server performs both authentication (TokenReview) and authorization
(SubjectAccessReview). Without a ClusterRoleBinding, Prometheus receives HTTP 401
and the scaler query returns no data.

The WVA install creates a `wva-epp-metrics-reader-role` ClusterRole automatically.
Bind Prometheus's service account to it:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: prometheus-epp-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: wva-epp-metrics-reader-role
subjects:
  - kind: ServiceAccount
    name: <prometheus-service-account>    # e.g. kube-prometheus-stack-prometheus
    namespace: <prometheus-namespace>     # e.g. workload-variant-autoscaler-monitoring
```

```bash
kubectl apply -f epp-rbac.yaml
```

### 3. Prometheus is scraping EPP

Create a `ServiceMonitor` with bearer token authentication:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: epp-metrics
  namespace: <namespace>
  labels:
    release: <prometheus-release-label>   # must match Prometheus operator's serviceMonitorSelector
spec:
  namespaceSelector:
    matchNames:
      - <namespace>
  selector:
    matchLabels:
      app.kubernetes.io/name: <epp-service-label>   # label on the EPP Service
  endpoints:
    - port: http-metrics                  # port name on the EPP Service (exposes 9090)
      path: /metrics
      scheme: http
      interval: 15s                       # match EPP's own poll frequency; reduces metric lag
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
```

```bash
kubectl apply -f epp-servicemonitor.yaml
```

Verify Prometheus is scraping EPP before proceeding:

```bash
kubectl port-forward svc/prometheus-operated 9090:9090 -n <prometheus-namespace>
# Query: inference_pool_average_kv_cache_utilization
# Query: inference_pool_average_queue_size
```

### 4. KEDA is installed

```bash
kubectl get deployment -n keda-system -l app.kubernetes.io/name=keda-operator
```

If not installed, use Helm:

```bash
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm upgrade -i keda kedacore/keda \
  --version 2.19.0 \
  --namespace keda-system --create-namespace \
  --set prometheus.metricServer.enabled=true \
  --set prometheus.operator.enabled=true \
  --wait
```

On the kind emulator, the WVA installer wires KEDA up for you:

```bash
KEDA_HELM_INSTALL=true SCALER_BACKEND=keda make deploy-e2e-infra
```

---

## Step 1 — Apply the ScaledObject

Save the following as `epp-saturation-scaledobject.yaml`. Replace the placeholder values marked with comments. KEDA queries Prometheus directly — there is nothing to register on the external metrics API.

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: epp-saturation-scaledobject
  namespace: <namespace>                      # replace
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: <deployment-name>                   # replace

  minReplicaCount: 1
  maxReplicaCount: 10                          # replace with your upper bound
  pollingInterval: 15                          # how often KEDA queries Prometheus

  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 0   # react immediately
          policies:
            # periodSeconds must be >= pod startup time. Set it to how long your pod
            # takes to become Ready — the HPA waits this long before adding another replica.
            #
            # GPU hardware (H100/A100, real model load):  periodSeconds: 180
            # Simulator / kind cluster (fast startup):    periodSeconds: 30
            - type: Pods
              value: 1
              periodSeconds: 180          # replace with actual pod startup time
          selectPolicy: Max

        scaleDown:
          # How long all metrics must stay below target before scaling down.
          # GPU hardware: 300 s (5 min) — conservative, avoids flapping under bursty traffic
          # Simulator / kind cluster: 60 s — sufficient for validation
          stabilizationWindowSeconds: 300 # replace with 60 for simulator/kind
          policies:
            - type: Pods
              value: 1
              periodSeconds: 300          # replace with 60 for simulator/kind
          selectPolicy: Min

  triggers:
    # Pool-average KV cache utilization (0.0–1.0), reported by EPP
    - type: prometheus
      name: kv-cache
      metricType: AverageValue
      metadata:
        serverAddress: <prometheus-url>   # e.g. https://kube-prometheus-stack-prometheus.<prometheus-namespace>.svc.cluster.local:9090
        # max() collapses the result to a single scalar so KEDA never errors on
        # multiple series (e.g. more than one EPP replica scraped).
        query: |
          max(inference_pool_average_kv_cache_utilization{name="<inference-pool-name>",namespace="<namespace>"})
        threshold: "0.7"                  # scale up when pool avg exceeds 0.70 per replica
        activationThreshold: "0"
        unsafeSsl: "true"                 # kube-prometheus-stack serves HTTPS with a self-signed cert

    # Pool-average waiting request queue length, reported by EPP
    - type: prometheus
      name: queue-size
      metricType: AverageValue
      metadata:
        serverAddress: <prometheus-url>
        query: |
          max(inference_pool_average_queue_size{name="<inference-pool-name>",namespace="<namespace>"})
        threshold: "2"                    # scale up when pool avg exceeds 2 per replica
        activationThreshold: "0"
        unsafeSsl: "true"
```

Apply it:

```bash
kubectl apply -f epp-saturation-scaledobject.yaml
```

---

## Step 2 — Verify the ScaledObject is working

KEDA creates an HPA named `keda-hpa-<scaledobject-name>`. Check both:

```bash
kubectl get scaledobject epp-saturation-scaledobject -n <namespace>
kubectl get hpa keda-hpa-epp-saturation-scaledobject -n <namespace>
```

Expected output when metrics are flowing:
```
NAME                          SCALETARGETKIND      SCALETARGETNAME       READY   ACTIVE
epp-saturation-scaledobject   apps/v1.Deployment   <deployment-name>     True    True

NAME                                   REFERENCE                     TARGETS          MINPODS   MAXPODS   REPLICAS
keda-hpa-epp-saturation-scaledobject   Deployment/<deployment-name>  450m/700m, 0/2   1         10        2
```

If the ScaledObject `READY` column is `False`, KEDA cannot reach Prometheus or the query returns no data — inspect it with:

```bash
kubectl describe scaledobject epp-saturation-scaledobject -n <namespace>
```

---

## Tuning thresholds

Adjust the `threshold` values in the ScaledObject triggers to change how aggressively the pool scales. Lower values scale up earlier (more headroom); higher values tolerate more saturation before adding replicas.

### Common preset configurations

| Profile | KV `threshold` | Queue `threshold` | When to use |
|---|---|---|---|
| **Default** | `0.7` | `2` | General workloads with moderate latency requirements |
| **Aggressive** (high GPU utilization) | `0.85` | `10` | Throughput-optimised; tolerate higher saturation to keep GPUs busy |
| **Conservative** (low-latency SLO) | `0.55` | `1` | Latency-sensitive workloads; scale up with more headroom |

Edit the ScaledObject in place to update thresholds:

```bash
kubectl edit scaledobject epp-saturation-scaledobject -n <namespace>
```

KEDA reconciles the change and updates the generated HPA — no pod restart needed.

---

## P/D disaggregated deployments

> **TODO**: Document ScaledObject setup for prefill/decode disaggregated deployments (one ScaledObject per Deployment).

---

## Local experiment with fake metrics

A self-contained experiment that exercises the full signal path on a kind cluster is available under `hack/experiments/epp-hpa/`. It uses the llm-d inference simulator with `--fake-metrics` to step the pool through four saturation scenarios and captures scaling events for each phase:

| Phase | Simulator config | Expected scaling action |
|---|---|---|
| 1 | `kv-cache=0.85, queue=0` | Scale up 1 → 2 (KV-driven) |
| 2 | `kv-cache=0.20, queue=0` | Scale down 2 → 1 |
| 3 | `kv-cache=0.30, queue=5` | Scale up 1 → 2 → 3 (queue-driven) |
| 4 | `kv-cache=0.10, queue=0` | Scale down 3 → 2 → 1 |

```bash
cd hack/experiments/epp-hpa
./run.sh up      # deploy simulator, EPP RBAC, ServiceMonitor, KEDA ScaledObject, and load generator job
./run.sh logs    # stream load generator output
./run.sh status  # check ScaledObject + generated HPA + deployment state
./run.sh down    # clean up all experiment resources
```

The experiment requires a running kind cluster with EPP, kube-prometheus-stack, and KEDA deployed (see `docs/developer-guide/testing.md`).
