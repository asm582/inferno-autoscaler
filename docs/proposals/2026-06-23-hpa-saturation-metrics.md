# HPA Autoscaling Based on vLLM Saturation Metrics

**Release target:** v0.9.0

---

## Context

WVA Saturation V1 is on the path to deprecation. As operators migrate away from it,
a plain Kubernetes HPA can replicate the same scaling behaviour by consuming vLLM
KV-cache and queue-depth metrics directly through Prometheus Adapter. The HPA
`averageValue` targets are derived from the V1 saturation parameters:

```
KV cache target    = kvCacheThreshold − kvSpareTrigger
Queue depth target = queueLengthThreshold − queueSpareTrigger
```

This produces the same scale-up trigger as V1 — the HPA fires when average utilization
exceeds the derived target, which is equivalent to spare capacity falling below the
spare trigger. HPA's max-of-all-metrics behaviour mirrors V1's OR logic across the two
signals.

## Scope

This spec defines the annotation schema that identifies an HPA as using the Sat V1 HPA
pattern and declares which metric names in its `spec.metrics` correspond to KV cache
and queue depth respectively. No new runtime component is introduced.

## Configuration

### HPA annotations

| Annotation | Required | Description |
|---|---|---|
| `llm-d.ai/saturation-hpa: "true"` | yes | Marks this HPA as using the Sat V1 HPA pattern. |
| `llm-d.ai/model-id: "<modelID>"` | yes | Model ID for this HPA's workload (e.g. `ibm/granite-13b`). |
| `llm-d.ai/kv-cache-metric: "<name>"` | no | Name of the `External` metric in `spec.metrics` whose `averageValue` is set to `kvCacheThreshold − kvSpareTrigger`. |
| `llm-d.ai/queue-depth-metric: "<name>"` | no | Name of the `External` metric in `spec.metrics` whose `averageValue` is set to `queueLengthThreshold − queueSpareTrigger`. |

At least one of `llm-d.ai/kv-cache-metric` or `llm-d.ai/queue-depth-metric` must be
present.

### Example

Using V1 defaults (`kvCacheThreshold: 0.80`, `kvSpareTrigger: 0.10`,
`queueLengthThreshold: 5`, `queueSpareTrigger: 3`):

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-saturation-hpa
  namespace: <namespace>
  annotations:
    llm-d.ai/saturation-hpa: "true"
    llm-d.ai/model-id: "ibm/granite-13b"
    llm-d.ai/kv-cache-metric: "vllm_kv_cache_usage_perc"
    llm-d.ai/queue-depth-metric: "vllm_num_requests_waiting"
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: <vllm-deployment-name>
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: External
      external:
        metric:
          name: vllm_kv_cache_usage_perc
          selector:
            matchLabels:
              model_name: "ibm/granite-13b"
        target:
          type: AverageValue
          averageValue: "700m"   # kvCacheThreshold(0.80) − kvSpareTrigger(0.10)
    - type: External
      external:
        metric:
          name: vllm_num_requests_waiting
          selector:
            matchLabels:
              model_name: "ibm/granite-13b"
        target:
          type: AverageValue
          averageValue: "2"      # queueLengthThreshold(5) − queueSpareTrigger(3)
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
        - type: Pods
          value: 1
          periodSeconds: 180
      selectPolicy: Max
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
        - type: Pods
          value: 1
          periodSeconds: 300
      selectPolicy: Min
```
