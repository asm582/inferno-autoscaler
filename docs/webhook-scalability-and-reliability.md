# Webhook Scalability and Reliability Analysis

## Executive Summary

**Can webhooks scale?** ✅ Yes, horizontally without limit (no leader election required)

**Is this a SPOF (Single Point of Failure)?** ⚠️ **Not with proper HA setup**, but single replicas are high-risk

**Critical constraint:** Webhooks are **synchronous**—API server waits for webhook response. Pod eviction throughput is limited by webhook latency × API server concurrency slots.

---

## Scalability Characteristics

### Horizontal Scaling Architecture

Webhooks scale horizontally differently from Kubernetes controllers:

```
┌──────────────────────────────────────────────────────┐
│ Kubernetes API Servers (N instances)                 │
│  ├─ Each resolves webhook Service DNS → ClusterIP    │
│  ├─ Each establishes HTTP/1.1 or HTTP/2 connections │
│  └─ Each independently load-balances to webhook pods │
└──────────────────────────────────────────────────────┘
                         ↓ (HTTP)
┌──────────────────────────────────────────────────────┐
│ Webhook Service (ClusterIP)                          │
│  └─ kube-proxy iptables/IPVS load balancing          │
└──────────────────────────────────────────────────────┘
           ↓            ↓            ↓
    ┌──────────┐  ┌──────────┐  ┌──────────┐
    │ Webhook  │  │ Webhook  │  │ Webhook  │
    │  Pod-1   │  │  Pod-2   │  │  Pod-3   │
    └──────────┘  └──────────┘  └──────────┘
```

**Key points:**
- ✅ No leader election required (unlike controllers with single active instance)
- ✅ Each API server independently load-balances across all ready webhook pods
- ✅ HTTP keep-alive maintains connection pools across replicas
- ✅ Round-robin distribution at Service level
- ✅ Can scale to any number of replicas (tested up to 100+ in production)

### Recommended Replica Count for HA

**Minimum production: 3 replicas**

- **1 replica**: ❌ SPOF risk—pod crash breaks cluster operations
- **2 replicas**: ⚠️ Better but still risky; node maintenance can take down both
- **3 replicas**: ✅ Handles node failures, pod evictions, maintenance rolling updates
- **5+ replicas**: ✅ For high-traffic clusters or to reduce per-pod CPU impact

### Resource Efficiency

Webhooks scale cheaply compared to controllers:

| Component | Scaling Model | Resource Growth |
|-----------|---------------|-----------------|
| **Webhook** | Horizontal (no leader election) | Linear: N replicas = N×base resource |
| **Controller** | Vertical (single active) + leader election | Sublinear: added overhead for election |
| **Operator** | Often vertical | Steep: single pod must handle all reconciliation |

**For your pod selection webhook:**
- Memory per pod: ~128Mi (very lightweight)
- CPU per pod: 50m-100m under normal load
- Scaling from 3→10 replicas costs ~$5-10/month in a typical cluster

---

## Failure Modes and SPOF Analysis

### Is This a Single Point of Failure?

**Short answer:** Not with proper HA setup, but architecture creates risks:

```
Risk Levels:
├─ 1 replica:           🔴 CRITICAL SPOF
├─ 2 replicas:          🟠 HIGH (node maintenance fails both)
├─ 3 replicas + HA:     🟡 MEDIUM (managed risks)
├─ 5+ replicas + HA:    🟢 LOW (resilient)
└─ 0 replicas (down):   🔴 PARTIAL OUTAGE (depends on failurePolicy)
```

### failurePolicy Behavior (CRITICAL)

The webhook configuration has a **failurePolicy** field that determines cluster behavior on webhook failure:

**Option 1: `failurePolicy: Fail` (DEFAULT)**

```yaml
webhooks:
- name: pod-selection.inferno.dev
  failurePolicy: Fail  # ← DEFAULT
  ...
```

Behavior:
- Webhook unreachable → admission request **REJECTED**
- Webhook times out → admission request **REJECTED**
- Webhook returns error → admission request **REJECTED**

**Cluster impact:**
```
Webhook down + failurePolicy: Fail
    ↓
All eviction requests denied
    ↓
HPA cannot scale down
    ↓
Pod replicas stuck at current count
    ↓
Cluster may hit resource limits if scale-down needed
```

**Severity:** 🔴 **CRITICAL**—cluster operations partially broken

**Option 2: `failurePolicy: Ignore`**

```yaml
webhooks:
- name: pod-selection.inferno.dev
  failurePolicy: Ignore
  ...
```

Behavior:
- Webhook unreachable → admission request **ALLOWED** (webhook failure ignored)
- Webhook times out → admission request **ALLOWED**
- Webhook returns error → admission request **ALLOWED**

**Cluster impact:**
```
Webhook down + failurePolicy: Ignore
    ↓
Eviction requests allowed (bypass webhook)
    ↓
HPA scales down pods (but WITHOUT pod cost awareness)
    ↓
May evict loaded pods (request loss possible)
    ↓
Cluster continues operating
```

**Trade-off:** Availability > Policy enforcement. No request loss guarantee, but cluster stays operational.

**Recommendation for your deployment:**
```yaml
# Current config (in config/webhook/mutatingwebhook.yaml):
failurePolicy: Ignore  # Prefer availability over perfect pod selection

# Rationale:
# - Pod selection is an optimization, not a safety requirement
# - If webhook fails, random pod eviction is acceptable
# - Better to lose pod selection than break autoscaling entirely
```

### Timeout Behavior

**Default: `timeoutSeconds: 10`** (from Kubernetes admission control)

```yaml
webhooks:
- name: pod-selection.inferno.dev
  timeoutSeconds: 5  # Keep SHORT
```

**Timeout impact on cluster:**

```
API Server Concurrency Slots (200 default for mutations)

Scenario 1: Fast webhook (50ms)
  ├─ Request occupies slot for 50ms
  ├─ 200 slots × 50ms = 10 seconds per full rotation
  ├─ Throughput: ~20 evictions/sec

Scenario 2: Slow webhook (1s)
  ├─ Request occupies slot for 1 second
  ├─ 200 slots × 1s = 200 seconds per full rotation
  ├─ Throughput: ~2 evictions/sec
  └─ Result: Cluster severely bottlenecked

Scenario 3: Hung webhook (10s timeout, then failure)
  ├─ Request occupies slot for 10 seconds
  ├─ 200 slots × 10s = 2000 seconds per full rotation
  ├─ Throughput: ~0.1 evictions/sec
  └─ Result: CLUSTER EFFECTIVELY BROKEN during webhook recovery
```

**Key lesson:** Slow webhooks don't just slow themselves—they block entire cluster operations

**Critical parameters:**
- Keep `timeoutSeconds` ≤ 5 seconds (ideally 3)
- Monitor actual webhook p99 latency
- Alert if latency approaches timeout threshold
- Never set timeout > 30 seconds (will completely block cluster)

### No Retry Behavior (CRITICAL)

**Important:** Kubernetes API server does NOT retry failed webhook requests

```
Webhook request fails
    ↓ (NO RETRY—immediate)
Failure applies failurePolicy
    ↓
- failurePolicy: Fail → admission REJECTED
- failurePolicy: Ignore → admission ALLOWED
```

**Implications:**
- Transient network glitch → admission fails (not retried)
- Webhook pod restarting → requests fail for 30-60 seconds
- No exponential backoff
- No circuit breaker

**Consequence for pod eviction:**
```
Webhook unavailable for 60 seconds
    ↓
All 60 seconds of eviction requests: REJECTED (if failurePolicy: Fail)
    ↓
HPA scale-down blocked for full minute
    ↓
Cluster may accumulate pending requests
```

**Mitigation:**
- Use `failurePolicy: Ignore` to allow evictions to proceed without webhook
- Monitor webhook availability continuously
- Set aggressive pod restart thresholds to detect failures early
- Use readiness probes to quickly detect failure

---

## Performance Characteristics

### Maximum Throughput

**Pod eviction throughput formula:**

```
Evictions/sec = (Mutating API slots) / (Webhook latency + overhead)
              = 200 / (latency_ms + 5ms)

Examples:
├─ Latency 10ms:   200 / 15ms   = 13 evictions/sec
├─ Latency 50ms:   200 / 55ms   = 3.6 evictions/sec
├─ Latency 100ms:  200 / 105ms  = 1.9 evictions/sec
└─ Latency 500ms:  200 / 505ms  = 0.4 evictions/sec
```

**For your webhook (queue-aware pod selection):**

Expected latencies:
- Webhook receive + deserialize: ~1ms
- Query EPP: ~50ms (network call to EPP service)
- Calculate costs: ~2ms
- Patch pods: ~20ms (Kubernetes API call)
- Serialize response: ~1ms
- **Total: ~75ms per eviction**

**Resulting throughput:** 200 / 75ms ≈ **2.6 evictions per second**

**Scaling implications:**
- 3 replicas → ~8 evictions/sec cluster-wide
- 10 replicas → ~26 evictions/sec cluster-wide
- 30 replicas → ~78 evictions/sec cluster-wide

**Real-world context:**
- Most KEDA scale-down is gentle: 1-2 evictions per minute
- Even 1 replica provides sufficient throughput for typical inference workloads
- High concurrency (hundreds of pods evicting simultaneously) is rare

### Latency Impact on Cluster

Webhook latency compounds with other API operations:

```
Example cluster operations affected:
├─ Pod creation: +75ms per admission request
├─ Pod updates: +75ms per update
├─ ConfigMap changes: not affected (webhook only on Pods)
└─ API server queries: affected if concurrent with evictions
```

**For inference workloads (typically pod creations are infrequent):**
- Pod startup: hundreds of milliseconds anyway
- +75ms for webhook = ~7-10% overhead
- Negligible user impact

### Connection Pooling

- Kubernetes API server maintains HTTP keep-alive connections to webhook Service
- Connections are **NOT reset per request**—reused across evictions
- Connection pool size determined by API server internals (typically 100+ connections)
- **Benefit:** Minimal connection overhead; latency is dominated by actual processing

---

## Production Reliability Setup

### HA Deployment Checklist

Your deployment must have ALL of these for production:

**Replica & Scheduling**

```yaml
replicas: 3                    # ✅ Required: minimum HA

affinity:
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:  # REQUIRED
    - labelSelector:
        matchLabels:
          app: inferno-webhook
      topologyKey: kubernetes.io/hostname  # No two pods on same node

topologySpreadConstraints:  # ✅ Recommended: spread across zones
- maxSkew: 1
  topologyKey: topology.kubernetes.io/zone
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
      app: inferno-webhook
```

**Pod Disruption Budget**

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: inferno-webhook-pdb
spec:
  minAvailable: 1           # ✅ Always keep at least 1 pod running
  selector:
    matchLabels:
      app: inferno-webhook

  # Allows node maintenance without breaking cluster
  # Example: 3 pods → 1 can be evicted → 2 remain operational
```

**Health Probes**

```yaml
readinessProbe:
  httpGet:
    path: /readyz          # ✅ Required: readiness
    port: 9443
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3
  # Result: Pod marked NotReady after 30 seconds of failures

livenessProbe:
  httpGet:
    path: /livez           # ✅ Recommended: liveness
    port: 9443
  initialDelaySeconds: 15
  periodSeconds: 20
  failureThreshold: 3
  # Result: Pod restarted after 60 seconds of failures
```

**timeoutSeconds Configuration**

```yaml
webhooks:
- name: pod-selection.inferno.dev
  timeoutSeconds: 5              # ✅ CRITICAL: Keep short
  failurePolicy: Ignore          # ✅ CRITICAL: Allow fallback on failure
  admissionReviewVersions: ["v1"]
  sideEffects: None
```

### Monitoring and Alerting

**Essential metrics to collect:**

```
# Webhook latency (per operation)
webhook_request_duration_seconds
  histogram with buckets: [0.01, 0.05, 0.1, 0.5, 1.0]
  labels: operation (query_epp, patch_pod, etc.)

# Admission decisions
webhook_admission_decisions_total
  counter
  labels: decision (allowed, denied)

# Webhook availability
webhook_ready
  gauge (1 = ready, 0 = not ready)

# Pod readiness changes
kube_pod_status_ready_condition
  should stay at 2 or higher for webhook deployment
```

**Alerts (critical):**

```
Alert: WebhookUnavailable
condition: count(kube_pod_status_ready{app="inferno-webhook"}) < 1
severity: CRITICAL
action: Page on-call immediately

Alert: WebhookLatencyHigh
condition: histogram_quantile(0.99, webhook_request_duration_seconds) > 3s
severity: WARNING
action: Investigate slow responses; may need replicas

Alert: WebhookAdmissionDenied
condition: rate(webhook_admission_decisions_total{decision="denied"}[5m]) > 0
severity: WARNING
action: Investigate; may indicate webhook misconfigurations
```

---

## Scaling Your Deployment: Step-by-Step

### Start: Baseline (3 replicas)

```yaml
# config/webhook/deployment.yaml
replicas: 3
# + podAntiAffinity (required)
# + PDB minAvailable: 1
# + readinessProbe
# + timeoutSeconds: 5
```

**Expected performance:**
- Eviction throughput: ~8 per second (2.6 per replica)
- Sufficient for typical KEDA workloads
- Resource cost: 3 × (100m CPU, 128Mi memory) = 300m CPU

### Monitor Phase (1-2 weeks)

Collect metrics:
- Actual webhook latency (p50, p95, p99)
- Eviction throughput during scale-down events
- Pod restart frequency
- API server queue depth

### Scale Up If Needed

If metrics show bottleneck:

```yaml
replicas: 5           # Increase if:
                      # - Webhook latency > 500ms consistently
                      # - Eviction throughput < 1 per second
                      # - API server mutating queue depth > 100
```

**Maximum recommended:** 10 replicas
- Beyond 10, returns diminish (load balancing overhead)
- Better to optimize webhook latency instead

---

## Known Limitations and Gotchas

### Gotcha 1: SCC Re-evaluation (OpenShift)

On OpenShift, if webhook mutates pod security settings:
1. SCC evaluation → assigns UID
2. Webhook mutation → may conflict with SCC decisions
3. SCC re-evaluation → may reject pod

**Your mitigation:** Webhook only patches annotations, not security settings → no SCC re-evaluation

### Gotcha 2: Self-mutation Loops

If webhook mutates something that triggers the same webhook:
```
Webhook adds label
    ↓ triggers another admission request
    ↓ for same pod
    ↓ webhook adds label again
    ↓ ...infinite loop
    ↓ request hangs until timeout
```

**Your mitigation:** Webhook only patches annotations AFTER eviction decision is made → no re-trigger

### Gotcha 3: Network Policy Breakage

If network policy blocks webhook Service from kube-apiserver:

```
Network policy denies:
  FROM: kube-apiserver pods
  TO: webhook Service
    ↓
All webhook requests fail (network error)
    ↓
Behaves same as webhook down
    ↓
failurePolicy determines outcome
```

**Mitigation:** Test network policies thoroughly before production

---

## Comparison with Other Production Webhooks

### cert-manager (Certificate webhook)

- **Scale:** 3 replicas recommended in docs
- **Availability:** Ignores cert-manager admission denials on webhook failure
- **Concurrency:** Low—certificates are not created frequently
- **Production deployments:** Used by millions of pods; proven reliable at scale

### Istio (Sidecar injection webhook)

- **Scale:** 3+ replicas; tested up to 100+
- **Availability:** failurePolicy: Fail (critical path); must be highly available
- **Concurrency:** Very high—every Pod creation hits this webhook
- **Industry practice:** Requires aggressive monitoring; any downtime causes pod creation failures

### Linkerd (Service mesh injection)

- **Similar to Istio:** Pod anti-affinity, PDB required
- **Production consensus:** HA webhook is non-negotiable for data plane operations

---

## Final Recommendation

**For your pod selection webhook:**

✅ **Production HA Configuration**

```yaml
# Deployment
replicas: 3
podAntiAffinity: required
topologySpreadConstraints: across zones
podDisruptionBudget: minAvailable 1

# Webhook config
failurePolicy: Ignore
timeoutSeconds: 5

# Monitoring
Collect webhook latency, admission decisions, pod readiness
Alert on: WebhookUnavailable, latency > 3s

# Scaling guidance
Start with 3 replicas
Monitor for 2 weeks
Scale to 5-10 if needed (rare for inference workloads)
```

**This setup:**
- ✅ Handles node failures without cluster disruption
- ✅ Allows pod evictions even if webhook is down (failurePolicy: Ignore)
- ✅ Provides ~8 evictions/second (sufficient for typical workloads)
- ✅ Avoids SPOF with proper HA discipline
- ✅ Follows industry-proven patterns (cert-manager, Istio, Linkerd)

**Is it a SPOF with this setup?** No—but keep replicas ≥ 3 and monitor availability.
