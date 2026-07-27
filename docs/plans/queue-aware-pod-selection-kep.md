# Proposal: Queue-Aware Pod Selection During Scale-Down

**Status:** Proposal  
**Date:** 2026-07-26  
**Authors:**  
**Reviewers:** TBD  

---

## Summary

When Kubernetes HPA/KEDA scales down inference workloads, it randomly selects pods for eviction regardless of their queue depth or in-flight request count. This leads to request loss, timeout errors, and poor user experience during expected scale-down events.

This proposal introduces a **Pod Deletion Cost Controller** that:
1. Watches KEDA `ScaledObject` for scale-down signals
2. Queries EPP (Endpoint Picker) for per-pod queue metrics
3. Calculates intelligent deletion costs based on queue depth and running requests
4. Patches pods with `controller.kubernetes.io/pod-deletion-cost` annotation
5. Allows ReplicaSet to evict idle pods first during scale-down

Additionally, we propose an **EPP Deletion-Aware Filter Plugin** to prevent new requests from routing to pods marked for termination.

---

## Goals

### Primary Goals
- **Avoid request loss during scale-down:** Ensure idle pods (with zero pending requests) are evicted before busy pods
- **Reduce operational errors:** Lower timeout and connection errors during KEDA scale-down events
- **Work with any KEDA scaler:** Solution must be scaler-agnostic (Prometheus, external scalers, custom, etc.)

### Secondary Goals
- **Minimal API server overhead:** Controller should patch pods only when scale-down events occur, not continuously
- **Observable and debuggable:** Clear logging and metrics for troubleshooting
- **Optional opt-in:** Can be disabled without breaking existing deployments

---

## Non-Goals

- **Replace HPA/KEDA logic:** We do not modify or replace Kubernetes' native HPA or KEDA's scaling decisions
- **Guaranteed request completion:** Cannot guarantee all in-flight requests complete (Kubernetes forcefully kills pods after grace period)
- **Application-level request draining:** Application must implement graceful shutdown (SIGTERM handling); this proposal does not add new draining logic
- **Per-request SLA guarantees:** Cannot promise specific latency bounds; only optimizes for available graceful termination window
- **Support for non-queue-based workloads:** Designed for queue-driven inference workloads; may not apply to other patterns
- **Automatic rollback or failure recovery:** Manual intervention required if deletion-cost calculation fails
- **Cross-variant coordination:** Does not handle complex scale-down across multiple model variants

---

## Motivation

### Problem Statement

Consider a 5-pod inference cluster during KEDA scale-down:

```
Current State:
  Pod A: queue_depth=50, running_requests=10  (BUSY, should NOT evict)
  Pod B: queue_depth=25, running_requests=5   (MODERATE, prefer not)
  Pod C: queue_depth=0,  running_requests=0   (IDLE, best candidate)
  Pod D: queue_depth=0,  running_requests=0   (IDLE, best candidate)
  Pod E: queue_depth=30, running_requests=8   (BUSY, should NOT evict)

KEDA Decision:
  Total queue = 105 requests
  Scale down to 4 replicas (delete 1 pod)

Current HPA Behavior:
  Randomly picks Pod A, B, C, D, or E (50% chance of evicting a BUSY pod)
  Pod evicted → may lose pending/running requests ✗

With Queue-Aware Pod Deletion Cost:
  Controller queries EPP, identifies Pod C and D have NO waiting requests
  Controller sets high deletion cost on busy pods (A, B, E)
  Controller sets low deletion cost on idle pods (C, D)
  
  ReplicaSet reads costs and evicts Pod C first
  Pod C: no pending work → terminates immediately ✓
  Pod A: has pending work → stays running, can finish processing in good faith ✓
```

**Impact:**
- Random eviction: ~40% chance of evicting a busy pod → request loss, errors
- Queue-aware eviction: Guides ReplicaSet/StatefulSet to prioritize idle pods for eviction → reduced request loss

### Use Cases

1. **LLM Inference Servers:** During traffic dips, scale-down shouldn't interrupt active token generation
2. **Batch Processing:** Queue-based workloads benefit from intelligent eviction order
3. **Multi-Variant Deployments:** When scaling individual model replicas, avoiding busy pods is critical

---

## Design

### High-Level Architecture

```
KEDA ScaledObject (via Prometheus metrics)
    ↓
Detects scale-down needed
    ↓
ScaledObject.status.desiredReplicas decreases
    ↓
HPA patches Deployment/StatefulSet.spec.replicas
    ↓
Pod Deletion Cost Controller watches ScaledObject
    ↓
Controller detects: desiredReplicas < currentReplicas
    ↓
Controller Action:
  1. Query EPP for queue depth per pod
  2. Calculate cost: cost = (queue×10) + (running×5) - 100
  3. Patch pods: controller.kubernetes.io/pod-deletion-cost
    ↓
ReplicaSet/StatefulSet reads pod-deletion-cost annotation
    ↓
ReplicaSet/StatefulSet evicts lowest-cost pods first (idle pods) ✓
```

### Components

#### 1. Pod Deletion Cost Controller

**Location:** `cmd/deletion-cost-controller/` in inferno-autoscaler

**Responsibilities:**
- Watch `ScaledObject` CRD for scale-down signals
- Query EPP `/endpoints` API for per-pod metrics
- Calculate deletion costs using formula
- Patch pods with `controller.kubernetes.io/pod-deletion-cost` annotation
- Handle errors gracefully (best-effort patching)

**Key Properties:**
- **Trigger:** `ScaledObject.status.desiredReplicas < status.currentReplicas`
- **Frequency:** On-demand (only during scale-down events)
- **Latency:** Target <100ms from detection to patching
- **API Load:** ~10 patch operations per scale-down event (not continuous)


## Detailed Design

### Pod Deletion Cost Calculation

**Formula:**
```
cost = (queue_depth × 10) + (running_requests × 5) - 100
```

**Rationale:**
- `queue_depth × 10`: Pending requests are primary eviction blocker (highest weight)
- `running_requests × 5`: Active requests should be protected but less than queue
- `-100`: Baseline allows idle pods to have negative costs (prioritized for eviction)

**Examples:**
```
Pod with queue=50, running=10:  cost = (50×10) + (10×5) - 100 = 450 (PROTECT)
Pod with queue=0,  running=0:   cost = (0×10)  + (0×5)  - 100 = -100 (EVICT)
Pod with queue=10, running=2:   cost = (10×10) + (2×5)  - 100 = 0 (NEUTRAL)
```

**ReplicaSet Eviction Order:**
When scaling down, ReplicaSet/StatefulSet consults annotation in this order:
1. Pending/Unknown phase pods
2. Not-ready pods
3. **Pods with lowest `pod-deletion-cost` annotation** ← Our optimization
4. Pods with recent creation timestamp

### Compatibility & Flexibility

**Works with Multiple Autoscaling Approaches:**
- **EPP + KEDA:** Direct KEDA integration via Prometheus triggers
- **WVA:** When WVA emits metrics to KEDA
- **Coordinator:** When coordinator emits metrics to KEDA

The controller watches `ScaledObject` status changes, the universal abstraction layer where all scaling decisions converge.

---

### Controller Scope & Behavior

**Operational Scope:**
- **Only patches pods during scale-down:** Controller patches pods ONLY when `ScaledObject.status.desiredReplicas < currentReplicas`
- **No proactive patching:** Controller does NOT continuously patch all pods when stable
- **On-demand, event-driven:** Patching triggered by scale-down detection, not by reconciliation cycles

**Processing Model:**
- **In-memory cost calculations:** Controller calculates pod deletion costs in-memory, no persistence
- **No state storage:** No ConfigMaps, no databases, no side effects beyond pod annotation patches
- **Stateless:** Each scale-down event independently queries EPP and calculates costs
- **Lifecycle:** Pods receive annotations only during scale-down events. During scale-up, new pods are created without annotations (acceptable, no eviction during scale-up). On next scale-down, all pods (old and new) receive fresh cost annotations.

**Benefits:**
- Low API server load (patches only during scale events)
- Simple operational model (no external state to manage)
- Fast calculation (in-memory)
- Recovers gracefully from failures (just tries again on next scale event)

---

### Watch Mechanism

**Watch Target:** `ScaledObject` CRD (`keda.sh/v1alpha1`)

**Event Detection:**
```go
if ScaledObject.Status.DesiredReplicas < ScaledObject.Status.CurrentReplicas {
    // Scale-down event detected
}
```

**Why Watch ScaledObject?**
- Earliest signal in KEDA reconciliation chain (before HPA updates)
- Scaler-agnostic (works with any KEDA trigger)
- Gives clear, explicit scale-down intent

### KEDA Configuration

**Limitation:** Controller effectiveness depends on eviction batch size. When KEDA scales multiple pods simultaneously (e.g., 5 pods at once), pod deletion costs become stale by the time eviction occurs, reducing the optimization benefit. For best results, configure KEDA to evict pods gradually (1-2 at a time) rather than in large batches.

Controllers work best with explicit stabilization windows:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: inference-server-scaler
spec:
  scaleTargetRef:
    name: inference-deployment
    kind: Deployment
  minReplicaCount: 1
  maxReplicaCount: 10
  
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleDown:
          stabilizationWindowSeconds: 300    # Recommended: 300s
          policies:
          - type: Pods
            value: 1                         # Scale 1 pod at a time
            periodSeconds: 60                # Every 60 seconds
```

**Why Stabilization Window?**
- Prevents thrashing (rapid scale-up/down)
- Gives controller time to patch costs between scale events
- Dampens metric fluctuations

**Timeline with Stabilization:**
- T0-T300: HPA continuously re-evaluates, collects recommendations, applies conservative policies
- T60: First scale-down opportunity, controller patches pods
- T120: Second scale-down (if still needed), controller patches again
- Fresh costs for each scale event ✓

---

## Implementation Plan

### Phase 1: Scaffold & EPP Integration (Days 1-2)

- [ ] Create `cmd/deletion-cost-controller/main.go`
- [ ] Create `internal/deletion_cost/controller.go`
- [ ] Reuse existing `internal/webhook/epp.go` (EPP client)
- [ ] Unit tests for EPP metric queries

### Phase 2: ScaledObject Watching (Days 3-4)

- [ ] Implement `ScaledObject` watcher using controller-runtime
- [ ] Detect scale-down signal: `desiredReplicas < currentReplicas`
- [ ] Route to `handleScaleDown()` handler
- [ ] Unit tests for watcher logic

### Phase 3: Cost Calculation & Pod Patching (Days 5-7)

- [ ] Implement cost calculation function (reuse from webhook if available)
- [ ] Implement pod patching logic
- [ ] Handle patch failures gracefully
- [ ] Unit tests for cost calculation and patching

### Phase 4: Kubernetes Configuration (Days 8-9)

- [ ] Write Deployment manifest
- [ ] Write RBAC (ServiceAccount, ClusterRole, ClusterRoleBinding)
- [ ] Write MutatingWebhookConfiguration (if using webhook for TLS cert injection)
- [ ] Document helm values if using Helm

### Phase 5: Testing & Documentation (Days 10-11)

- [ ] E2E test: KEDA scale-down with controller
- [ ] Integration test with envtest
- [ ] Performance test: API call overhead
- [ ] Write operator guide
- [ ] Write troubleshooting guide

---

## Testing Strategy

### Unit Tests
- Cost calculation for various queue/running combinations
- ScaledObject watcher trigger detection
- Pod patching success/failure scenarios

### Integration Tests (envtest)
- Full controller reconciliation loop
- ScaledObject watch with pod patching
- Error handling (EPP unavailable, patch failures)

### E2E Tests (Kind cluster)
- KEDA + HPA + controller integration
- Verify pods evicted in cost order
- Verify requests avoid terminating pods (with EPP filter)

### Performance Tests
- API server load (patch operation throughput)
- Controller latency (detection to patching)
- EPP query latency impact

---

## Configuration

### Controller Flags

```
--epp-url=http://epp:8080            # EPP service URL
--namespace=default                  # Namespace to watch
--log-level=info                     # Log level (debug, info, warn, error)
--sync-period=30s                    # Controller sync period
```

### Environment Variables

```
WATCH_NAMESPACE                      # Alternative to --namespace flag
KEDA_API_TIMEOUT                     # Timeout for KEDA API calls
EPP_QUERY_TIMEOUT                    # Timeout for EPP queries
```

---

## Risk Analysis

### Risk 1: EPP Unavailable During Scale-Down

**Scenario:** EPP service is down when controller tries to query metrics.

**Mitigation:**
- Use cached costs from last successful query (fallback)
- Log error but continue (best-effort)
- HPA falls back to random eviction (acceptable degradation)
- Monitor EPP availability; alert on unavailability

**Impact:** Graceful degradation to random eviction.

---

### Risk 2: Pod Patch Failures

**Scenario:** API server rejects patch (quota, permission, transient error).

**Mitigation:**
- Retry patches with exponential backoff
- Log patch failures with pod name for debugging
- Do not block scale-down (best-effort)
- Monitor patch failure rate

**Impact:** Some pods may not have costs set; HPA uses random selection for them.

---

### Risk 3: Stale Metrics During Scale-Down

**Scenario:** Queue depth changes between EPP query and actual eviction.

**Mitigation:**
- Query EPP on every scale-down event (not cached)
- Latency target: <100ms from detection to patching
- Stabilization window gives time for relatively fresh costs
- Graceful termination handles in-flight requests anyway

**Impact:** May evict slightly non-optimal pod in edge cases, but still better than random.

---

### Risk 4: High API Server Load

**Scenario:** Frequent scale-down events cause excessive patch operations.

**Mitigation:**
- Batch pod patches per scale-down event (not per-pod)
- On-demand patching (only during scale-down, not continuously)
- Monitor API server metrics during deployment

**Impact:** Expected API load <1 call/sec average (acceptable).

---

### Risk 5: Interaction with Other Pod Disruption Controls

**Scenario:** PodDisruptionBudget or other policies conflict with eviction.

**Mitigation:**
- Controller respects existing PDB policies (no direct eviction)
- Pod costs are hints to HPA; PDB is hard constraint
- Document interaction in operator guide

**Impact:** PDB takes precedence; eviction may be delayed.

---

## Success Criteria

### Functional Criteria
- [ ] Controller successfully watches ScaledObject changes
- [ ] Pod costs calculated correctly per formula
- [ ] Pods patched with deletion cost annotation
- [ ] HPA respects pod costs during eviction
- [ ] Idle pods evicted before busy pods in >90% of scale-down events

### Performance Criteria
- [ ] Controller patching latency: <100ms
- [ ] API server patch load: <1 call/sec average
- [ ] No impact on KEDA polling or HPA reconciliation

### Reliability Criteria
- [ ] Controller recovers from EPP unavailability
- [ ] Controller handles patch failures gracefully
- [ ] No resource leaks (memory, goroutines)
- [ ] Uptime: >99.5%

### Operational Criteria
- [ ] Documentation is complete and clear
- [ ] Helm chart provided for easy deployment
- [ ] Metrics/logs available for troubleshooting
- [ ] Can be disabled without breaking existing workloads

---

## Deployment

### Prerequisites

- Kubernetes 1.22+ (for `pod-deletion-cost` annotation support)
- KEDA 2.12+ installed and running
- EPP service accessible from controller pod
- ServiceAccount with appropriate RBAC

### Installation

```bash
# 1. Deploy controller
kubectl apply -f config/deletion-cost-controller/deployment.yaml
kubectl apply -f config/deletion-cost-controller/rbac.yaml

# 2. Or use Helm
helm install deletion-cost-controller ./chart/deletion-cost-controller \
  --namespace inferno-system \
  --set epp.url=http://epp:8080
```

### Verification

```bash
# Check controller is running
kubectl get pods -n inferno-system -l app=deletion-cost-controller

# Check logs
kubectl logs -n inferno-system -l app=deletion-cost-controller -f

# Scale down a deployment with KEDA
# Verify pods are patched with deletion costs
kubectl get pods -o jsonpath='{.items[*].metadata.annotations.controller\.kubernetes\.io/pod-deletion-cost}' | jq
```

---

## Related Work

- [Kubernetes KEP-2255: Pod Deletion Cost](https://github.com/kubernetes/enhancements/tree/master/keps/sig-apps/2255-pod-cost)
- [KEDA: Scaling Deployments](https://keda.sh/docs/latest/concepts/scaling-deployments/)
- [HPA Configurable Scaling Behavior](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#configurable-scaling-behavior)
- [lablabs/pod-deletion-cost-controller](https://github.com/lablabs/pod-deletion-cost-controller) (zone-aware distribution)

---

## Alternatives Considered

### Alternative 1: Webhook Admission Controller

**Approach:** Intercept eviction requests at the API server level and patch costs synchronously.

**Pros:**
- Synchronous (1-5ms race window)
- Atomic with eviction decision

**Cons:**
- TLS certificate management
- Admission timeout pressure (5s max)
- Adds complexity to cluster API path
- Requires additional infrastructure

**Decision:** Rejected in favor of simpler watch-based approach with acceptable trade-offs.

### Alternative 2: Continuous Pod Patching

**Approach:** Continuously poll EPP and patch pod costs every 10s regardless of scale-down events.

**Pros:**
- Always fresh costs

**Cons:**
- High API server load (20+ calls/sec)
- Unnecessary patching during stable state
- Costs still become stale between patches

**Decision:** Rejected in favor of on-demand patching.

### Alternative 3: StatefulSet Ordinals

**Approach:** Migrate from Deployment to StatefulSet; leverage reverse ordinal eviction order.

**Pros:**
- Deterministic eviction (no race)
- No custom logic needed

**Cons:**
- Breaking change for operators
- Less flexible (fixed eviction order)
- Requires application redesign

**Decision:** Rejected; recommended as alternative for future if applicable.

### Alternative 4: HPA Metric Manipulation

**Approach:** Fake higher metric values to prevent scale-down.

**Pros:**
- Simple

**Cons:**
- Violates system integrity
- Breaks monitoring/alerting
- Not scalable

**Decision:** Rejected.

---

**Document Version:** 1.0  
**Last Updated:** 2026-07-26  
**Status:** Ready for Review
