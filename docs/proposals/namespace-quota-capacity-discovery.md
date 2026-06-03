Namespace Quota Capacity Discovery

**Authors:** Abhishek Malvankar
**Status:** Provisional
**Created:** 2026-06-02
**Last Updated:** 2026-06-02

---

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Notes and Constraints](#notes-and-constraints)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API Changes](#api-changes)
  - [Activation](#activation)
  - [Accelerator Name Convention](#accelerator-name-convention)
- [Test Plan](#test-plan)
  - [Unit Tests](#unit-tests)
  - [Integration Tests](#integration-tests)
  - [E2E Tests](#e2e-tests)
- [Production Readiness Review](#production-readiness-review)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)

---

## Summary

Add `NamespaceQuotaDiscovery`, a new implementation of the existing `CapacityDiscovery`
interface that derives GPU capacity from Kubernetes `ResourceQuota` objects rather than
from GPU Operator node labels. When enabled via `WVA_CAPACITY_NAMESPACES=ns-a,ns-b,ns-c`,
the engine creates one quota-backed limiter per namespace and invokes the optimizer
separately for each namespace's models — so each namespace's quota ceiling is enforced
independently with no optimizer changes required.

---

## Motivation

The default capacity discovery backend (`K8sWithGpuOperator`) requires the
[NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/overview.html)
to label nodes with GPU model and memory information. This creates a hard dependency that
rules out a class of deployment environments:

- **Managed cloud services** (GKE, EKS, AKS) often do not expose GPU Operator labels on
  their managed node pools.
- **Multi-tenant clusters** commonly restrict node listing to cluster administrators,
  making per-node inventory inaccessible to namespace-scoped controllers.
- **Quota-governed namespaces** (common in OpenShift and enterprise on-prem clusters)
  already encode GPU capacity in `ResourceQuota.spec.hard` — this is the authoritative
  source and scanning nodes would duplicate it.

In each of these cases a namespace-scoped quota is both available and sufficient: the
quota hard limit already equals the GPU capacity visible to workloads in the namespace.

Beyond fixing the discovery problem, knowing a trustworthy total GPU ceiling is a
prerequisite for a broader class of fair-share problems. Consider two models served in the
same namespace, each initially using 10% of the cluster's GPUs. Model A's request volume
increases 10× and scales to 90%, leaving model B at 10%. When model B's request volume
subsequently increases 10×, the correct allocation is 50/50 — but a naive independent
autoscaler will not rebalance: model A still has a queue, so it sees no reason to release
GPUs. Solving this requires a redistributive optimizer that can force model A below its
current allocation even while it is still saturated. That optimizer cannot be built without
a reliable GPU ceiling to divide. This KEP provides that ceiling; the redistributive
optimizer is addressed in a follow-up proposal.

### Goals

1. Add a `NamespaceQuotaDiscovery` backend that reads GPU capacity from
   `ResourceQuota.spec.hard` and current usage from `ResourceQuota.status.used`.
2. Plug it into the existing `CapacityDiscovery` / `UsageDiscovery` interfaces with zero
   changes to `TypeInventory`, `DefaultLimiter`, or `GreedyByScoreOptimizer`.
3. Support multiple namespaces via `WVA_CAPACITY_NAMESPACES=ns-a,ns-b,ns-c` — one quota
   ceiling enforced independently per namespace.
4. Work on any Kubernetes distribution — GKE, EKS, AKS, OpenShift, kind — without
   requiring GPU Operator or node-level RBAC.

### Non-Goals

- Replacing `K8sWithGpuOperator` — it remains the default and is required for
  GPU-model-aware scheduling (e.g., differentiating A100 from H100).
- GPU-model-aware scheduling: all GPUs in a namespace are treated as one pool keyed by
  resource name (e.g. `nvidia.com/gpu`). Differentiating A100 from H100 within a
  namespace requires `K8sWithGpuOperator`.
- Dynamic reconfiguration without a controller restart.
- Supporting quota scopes (e.g., `BestEffort`, `Terminating`) — only the namespace-wide
  hard limit is read.

---

## Proposal

### User Stories

**Story 1 — Managed cloud operator**

> As a platform engineer running WVA on GKE Autopilot, I cannot access GPU Operator
> node labels, but my namespace has a `ResourceQuota` limiting `nvidia.com/gpu: 8`.
> I want the greedy optimizer to treat 8 as the total GPU budget so it does not
> over-schedule replicas beyond what the cluster can place.

**Story 2 — Quota-first operations**

> As a platform team that manages GPU capacity via quota rather than node labels,
> I want a single source of truth: the quota. I do not want to maintain GPU Operator
> labels in parallel.

### Notes and Constraints

- **Quota equals capacity.** This design treats `ResourceQuota.spec.hard` as the total
  GPU capacity available to workloads in the namespace. No node inventory, no GPU Operator
  labels — the quota is the single source of truth for how many GPUs can be scheduled.
  Keeping the quota aligned with actual node capacity is the operator's responsibility,
  the same way correct node labels are the operator's responsibility with `K8sWithGpuOperator`.

- **Accelerator key is the resource name, not the model name.** `ResourceQuota` stores
  `nvidia.com/gpu: 8`, not `NVIDIA-A100-PCIE-80GB: 8`. `TypeInventory` pools will be
  keyed by `nvidia.com/gpu`.

  **`VariantAutoscaling` is optional and will be deprecated.** When a VA is provided,
  `acceleratorName` must be set explicitly so it matches the GPU resource name in the
  `ResourceQuota` (e.g. `nvidia.com/gpu`).

- **Usage from `status.used`, not pod listing.** `ResourceQuota.status.used` is
  maintained by the quota admission controller and covers all admitted workloads in the
  namespace — including pending pods and non-WVA workloads — whereas
  `K8sWithGpuOperator.DiscoverUsage()` sums requests only from scheduled WVA-managed
  pods. Both are defensible; `status.used` is the more conservative count and the natural
  source of truth when quota is the capacity model.

- **Each namespace's quota is enforced independently.** A `ResourceQuota` only governs
  pods in its own namespace, so each namespace's ceiling must apply only to models running
  in that namespace. The engine achieves this without any optimizer changes by invoking
  the optimizer once per namespace with only that namespace's requests and constraint.
  Models in ns-a compete for ns-a's GPU quota; models in ns-b compete for ns-b's — there
  is no cross-namespace sharing.

- **Multiple quotas are aggregated.** If a namespace has more than one `ResourceQuota`
  object each quota is treated as a separate inventory entry (outer key in the
  `Discover()` map). `TypeInventory.Refresh()` sums them as it does for multiple nodes —
  no change to the aggregation logic is needed.

### Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `status.used` is stale immediately after quota creation | Low | Low | The admission controller updates `status.used` synchronously on pod admission; staleness is bounded by the API server response time, well within the 30-second optimization interval. |
| VA provided without `acceleratorName` set | Low | Low | When a VA is used, `acceleratorName` must be set. The optimizer skips unresolved variants and logs a warning rather than over-allocating. VA itself is optional and will be deprecated. |
| Mixed GPU types in one namespace share one pool | Low | Low | Documented as a known limitation. Use `K8sWithGpuOperator` for heterogeneous GPU namespaces. |

---

## Design Details

### New File: `internal/discovery/namespace_quota.go`

`NamespaceQuotaDiscovery` implements `FullDiscovery` by reading `ResourceQuota` objects.
It is a leaf implementation — it introduces no new interfaces, types, or abstractions.

```
internal/discovery/
  interface.go                 # unchanged
  types.go                     # unchanged
  k8s_with_gpu_operator.go     # unchanged
  namespace_quota.go           # NEW
  namespace_quota_test.go      # NEW
```

**`Discover(ctx) → map[quotaName]map[resourceName]AcceleratorModelInfo`**

1. List `ResourceQuotaList` in `d.Namespace`.
2. For each quota, scan `Spec.Hard` for GPU resource names (see table below).
3. Return each quota as its own outer key so `TypeInventory.Refresh()` can aggregate
   without any modification.

**`DiscoverUsage(ctx) → map[resourceName]int`**

1. List `ResourceQuotaList` in `d.Namespace`.
2. Aggregate `Status.Used` values for GPU resource names across all quotas.
3. Return the map. No pod listing. No inference.

**`DiscoverNodes(ctx) → map[string]NodeInfo`**

Returns an empty map. Quota-based discovery has no per-node information; callers that
require node affinity must use `K8sWithGpuOperator`.

**Interface compliance assertion:**

```go
var _ FullDiscovery = (*NamespaceQuotaDiscovery)(nil)
```

**GPU resource names recognized:**

| Resource name | Vendor |
|---|---|
| `nvidia.com/gpu` | NVIDIA |
| `requests.nvidia.com/gpu` | NVIDIA (prefixed form) |
| `amd.com/gpu` | AMD |
| `requests.amd.com/gpu` | AMD (prefixed form) |
| `intel.com/gpu` | Intel |
| `requests.intel.com/gpu` | Intel (prefixed form) |

### API Changes

No new CRDs, no new API types, no new flags in the saturation ConfigMap. The only user-
facing surface is the environment variable and the `acceleratorName` convention on VA specs.

### Activation

`WVA_CAPACITY_NAMESPACES` accepts a comma-separated list of namespaces. When set, the
engine creates one `NamespaceQuotaDiscovery` and one `DefaultLimiter` per namespace,
keyed by namespace name. When unset, the existing `K8sWithGpuOperator` path is used.

**`NewEngine()` — build per-namespace limiters:**

```go
// internal/engines/saturation/engine.go — NewEngine()
var quotaLimiters map[string]*pipeline.DefaultLimiter
if nsEnv := os.Getenv("WVA_CAPACITY_NAMESPACES"); nsEnv != "" {
    quotaLimiters = make(map[string]*pipeline.DefaultLimiter)
    for _, ns := range strings.Split(nsEnv, ",") {
        ns = strings.TrimSpace(ns)
        if ns == "" {
            continue
        }
        disc := discovery.NewNamespaceQuotaDiscovery(client, ns)
        inv  := pipeline.NewTypeInventoryWithUsage("quota-inventory-"+ns, disc)
        quotaLimiters[ns] = pipeline.NewDefaultLimiter("quota-limiter-"+ns, inv, pipeline.NewGreedyBySaturation())
    }
}
// if quotaLimiters is nil, existing K8sWithGpuOperator path is used unchanged
```

**Optimize loop — invoke optimizer once per namespace:**

```go
// internal/engines/saturation/engine.go — optimize()
if quotaLimiters != nil {
    byNamespace := groupRequestsByNamespace(requests)
    for ns, nsRequests := range byNamespace {
        var constraints []*pipeline.ResourceConstraints
        if limiter, ok := quotaLimiters[ns]; ok {
            usage := computeCurrentGPUUsage(nsRequests)
            if c, err := limiter.ComputeConstraints(ctx, usage); err != nil {
                logger.Error(err, "quota constraint failed, namespace runs unlimited", "namespace", ns)
            } else {
                constraints = append(constraints, c)
            }
        }
        decisions := e.optimizer.Optimize(ctx, nsRequests, constraints)
        allDecisions = append(allDecisions, decisions...)
    }
} else {
    // existing global constraint path — unchanged
}
```

The optimizer receives only the requests for one namespace at a time, so its internal
`available` map naturally reflects that namespace's quota ceiling. No changes to
`GreedyByScoreOptimizer` or `mergeConstraints` are required.

### Accelerator Name Convention

| Discovery mode | `TypeInventory` pool key | VA `acceleratorName` |
|---|---|---|
| `K8sWithGpuOperator` | `A100`, `H100` (normalized from node label) | `A100` |
| `NamespaceQuotaDiscovery` | `nvidia.com/gpu` (resource name, passed through) | `nvidia.com/gpu` |

`VariantAutoscaling` is optional and will be deprecated. When a VA is provided,
`acceleratorName` must be set so the limiter knows which pool to debit.

The difference in pool key convention from `K8sWithGpuOperator` is intentional:
`NormalizeAcceleratorName` strips vendor and memory suffixes from full GPU model names
(e.g. `NVIDIA-A100-PCIE-80GB` → `A100`); it is not applied to quota resource names
because there is no suffix to strip. `nvidia.com/gpu` passes through unchanged.

---

## Test Plan

### Unit Tests

**Location:** `internal/discovery/namespace_quota_test.go`

**Tool:** `controller-runtime/pkg/client/fake` — same pattern as
`k8s_with_gpu_operator_test.go`. No cluster required.

**Running:**
```bash
go test ./internal/discovery/... -run TestNamespaceQuota
```

**Note on fake client and `status.used`:** The fake client does not compute
`status.used` from `spec.hard`. Build `ResourceQuota` objects with both fields populated
explicitly and register them with `WithStatusSubresource()`.

```go
quota := &corev1.ResourceQuota{
    ObjectMeta: metav1.ObjectMeta{Name: "gpu-quota", Namespace: "llm-workloads"},
    Spec: corev1.ResourceQuotaSpec{
        Hard: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
    },
    Status: corev1.ResourceQuotaStatus{
        Used: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("3")},
    },
}
fakeClient := fake.NewClientBuilder().
    WithScheme(scheme).
    WithStatusSubresource(quota).
    WithRuntimeObjects(quota).
    Build()
```

#### `Discover()` coverage

| Test | Input | Expected |
|---|---|---|
| Single NVIDIA quota | `spec.hard["nvidia.com/gpu"]: 8` | `{"gpu-quota": {"nvidia.com/gpu": {Count:8}}}` |
| Two quotas, same namespace | Quota A: 4 GPUs, Quota B: 4 GPUs | Two outer keys; `TypeInventory.Refresh()` aggregates to 8 |
| No GPU entries in quota | Only CPU and memory limits | Empty map, no error |
| `requests.nvidia.com/gpu` prefix | Prefixed hard limit: 4 | Counted as `nvidia.com/gpu: 4` |
| Multi-vendor quota | `nvidia.com/gpu: 4`, `amd.com/gpu: 2` | Both keys present with correct counts |
| Empty namespace | No `ResourceQuota` objects | Empty map, no error |

For the two-quota case, instantiate a real `TypeInventory` (not a mock) and call
`Refresh()` to confirm that cross-quota aggregation works end-to-end at the unit level.
This is the only test that crosses a component boundary at unit level.

#### `DiscoverUsage()` coverage

| Test | Input | Expected |
|---|---|---|
| GPUs in use | `status.used["nvidia.com/gpu"]: 3` | `{"nvidia.com/gpu": 3}` |
| No usage | `status.used` empty or absent | Empty map, no error |
| Multi-vendor usage | Both `nvidia.com/gpu` and `amd.com/gpu` used | Both keys returned |
| Multiple quotas | Two quotas each with partial usage | Values summed by resource name |

### Integration Tests

Integration tests validate the full constraint pipeline — from `NamespaceQuotaDiscovery`
through `TypeInventory` → `DefaultLimiter.ComputeConstraints()` → `GreedyByScoreOptimizer`
— using a fake client and in-process components. No cluster required.

**Location:** `internal/engines/pipeline/` (alongside existing `type_inventory_test.go`)

**Running:**
```bash
go test ./internal/engines/pipeline/... -run TestQuotaConstraints
```

#### Coverage

These tests verify the discovery → inventory → constraints pipeline only. What the
optimizer does with the constraints is tested in `greedy_score_optimizer_test.go`.

| Test | What it validates |
|---|---|
| Hard limit read correctly | Quota `spec.hard["nvidia.com/gpu"]: 8`; `ComputeConstraints()` returns `ResourceConstraints{Pools: {"nvidia.com/gpu": {Limit:8}}}` |
| Usage read correctly | Quota `status.used["nvidia.com/gpu"]: 3`; `ComputeConstraints()` returns `{Limit:8, Used:3}` → `Available:5` |
| Zero hard limit | `spec.hard["nvidia.com/gpu"]: 0`; `ComputeConstraints()` returns `Available:0` |
| Usage at limit | `status.used` equals `spec.hard`; `ComputeConstraints()` returns `Available:0` |
| Two namespaces produce independent constraints | ns-a quota: 4 GPUs, ns-b quota: 8 GPUs; `ComputeConstraints()` called per namespace returns correct values independently |
| Quota discovery error | `Discover()` returns an error; `ComputeConstraints()` propagates it |

### E2E Tests

E2E tests run against a real cluster with the controller deployed. GPU hardware is not
required — `--fake-metrics` injects synthetic saturation signals.

**Location:** `test/e2e/quota_limiter_test.go`

**Labels:** `Label("quota-limiter", "full")` for all cases;
add `Label("smoke")` to the happy-path case if it completes in under 2 minutes.

**Running:**
```bash
# Full suite including quota limiter
make test-e2e-full

# Quota limiter tests only
make test-e2e-full -- --label-filter="quota-limiter"
```

#### Coverage

**Happy path — quota ceiling enforced:**
1. Apply namespace with `ResourceQuota` limiting `nvidia.com/gpu: 4`.
2. Deploy two `VariantAutoscaling` objects with `gpusPerReplica: 1` and no
   `acceleratorName` set (auto-resolved to `nvidia.com/gpu` by the limiter), each
   requesting replicas that would together exceed 4.
3. Enable `--fake-metrics` to drive both to saturation.
4. Assert `sum(TargetReplicas * gpusPerReplica) ≤ 4` across both VAs within one cycle.

**Quota updated at runtime:**
1. Start with `nvidia.com/gpu: 4` and two saturated VAs capped at 4 GPUs total.
2. `kubectl patch resourcequota gpu-quota --patch '{"spec":{"hard":{"nvidia.com/gpu":"8"}}}'`
3. Assert optimizer scales up to fill the new headroom within one optimization interval.

**Zero quota:**
1. Set `nvidia.com/gpu: 0` in the quota.
2. Assert no scale-up decisions are produced.

**Fallback to unlimited when quota discovery fails:**
1. Delete the `ResourceQuota` object mid-run.
2. Assert the controller logs a warning but continues producing decisions
   (falls back to unlimited mode, not a crash).

**Multi-namespace isolation:**
1. Set `WVA_CAPACITY_NAMESPACES=ns-a,ns-b` with `nvidia.com/gpu: 4` in ns-a and
   `nvidia.com/gpu: 8` in ns-b.
2. Saturate models in both namespaces.
3. Assert models in ns-a are capped at 4 GPUs total; models in ns-b are capped at 8.
4. Assert ns-a saturation does not affect ns-b headroom.

---

## Production Readiness Review

### Feature Enablement and Rollback

- **Enabled by:** setting `WVA_CAPACITY_NAMESPACES=ns-a,ns-b` on the controller Deployment.
- **Disabled by:** unsetting the variable and restarting the controller.
- **Rollback:** remove the env var; the controller reverts to `K8sWithGpuOperator`
  immediately on restart with no state to clean up.
- **Adding a namespace:** append it to the comma-separated list and restart.
- **Side effects of disabling:** the optimizer reverts to node-based capacity. If node
  capacity exceeds the quota that was previously enforced, the optimizer may produce
  larger scale-up decisions. This is expected and safe.

### Monitoring Requirements

- Existing `wva_available_gpus` metric continues to be emitted (populated from
  `TypeInventory.TotalAvailable()` regardless of discovery backend).
- Log line at `Info` level on every `Discover()` call: `"quota-based capacity discovered"`
  with `namespace`, `totalGPUs`, and `quotaCount` fields.
- Log line at `Warning` level when `Discover()` returns an empty map for a non-empty
  namespace (possible misconfiguration).

### Dependencies

- No new Go module dependencies. `ResourceQuota` is `core/v1`, already imported.
- **RBAC: add `resourcequotas` to `config/base/rbac/manager-clusterrole.yaml`.**
  The controller already uses a `ClusterRole` (not a namespaced `Role`) so a single
  addition covers all namespaces listed in `WVA_CAPACITY_NAMESPACES`. The verbs needed
  are read-only — `get`, `list`, `watch`. The exact stanza to add:

  ```yaml
  - apiGroups:
    - ""
    resources:
    - resourcequotas
    verbs:
    - get
    - list
    - watch
  ```

  `resourcequotas` is currently absent from the ClusterRole. This addition is part of
  the implementation, not optional post-deployment verification.

### Scalability

- `Discover()` issues one `ResourceQuotaList` API call per optimization cycle (default:
  30 seconds). A namespace with N quotas returns N objects in a single list response.
  This is strictly less expensive than the current `listGPUNodes()` which issues one
  API call per GPU vendor.
- No watch caching is added in this KEP; the existing client cache handles list responses.

---

## Relationship to Fair-Share Rebalancing

This KEP delivers quota-based capacity discovery. It is a prerequisite for fair-share
rebalancing across competing models but does not implement the rebalancing itself. This
section documents why, so a follow-up proposal can be scoped correctly.

### Why rebalancing is out of scope

Tracing through the concrete scenario from Motivation (100 total GPUs, Model A: 90
replicas, Model B: 10 replicas, both queues now growing equally):

1. **`computeCurrentGPUUsage`** sums current replicas for all models:
   `{"nvidia.com/gpu": 100}` (90 from A + 10 from B).

2. **`ComputeConstraints`** computes available = quota hard − used = 100 − 100 = **0**.

3. **`mergeConstraints`** gives the optimizer `{"nvidia.com/gpu": 0}` available GPUs.

4. Both models have `RequiredCapacity > 0` (each has a growing queue) so both enter
   `fairShareScaleUp`.

5. **`fairShareScaleUp` exits immediately** on the first check: `totalGPUs == 0 → break`.
   Nothing is allocated or reallocated.

6. `buildDecisionsWithOptimizer` produces targets equal to current replicas. Model A stays
   at 90, Model B stays at 10. **The rebalancing never happens.**

### Root cause

`GreedyByScoreOptimizer` is an **additive** optimizer: it distributes GPUs that are not yet
allocated. When the cluster is 100% utilized and both models are still saturated, available
= 0 and the fair-share loop has nothing to hand out.

The optimizer has no mechanism to **preempt** GPUs from a model that holds more than its
fair share and **redistribute** them to a peer that is equally or more starved. Scale-down
for a model only triggers when its `SpareCapacity > 0` (i.e., it has excess capacity
relative to its own demand). A model holding 90% of GPUs but still facing a growing queue
reports `SpareCapacity = 0` — it will never voluntarily release GPUs under the current
logic.

### What rebalancing actually requires

A redistributive fair-share pass that runs before or instead of the current additive pass
when the cluster is fully utilized:

1. **Compute a target fair-share per model.**
   When N models are all saturated, each is entitled to `total_GPUs / N`. Equal
   queue size (or equal time-in-queue for models with different work-unit durations) is the
   right fairness criterion — not equal request rate.

2. **Force scale-down models above their fair share**, even if their own queue is non-empty.
   Model A holds 90, fair share is 50: target = 50, regardless of its queue.

3. **Allocate freed GPUs to models below their fair share.**
   Model B holds 10, fair share is 50: target = 50, funded by Model A's release.

4. **Use queue size / time-in-queue as the balancing signal**, not just `RequiredCapacity`.
   Two models with equal queues should converge to equal GPU allocation. A model with a
   larger queue per GPU should get a larger share.

### What this KEP contributes

The fair-share formula (`total_GPUs / N`) requires a trustworthy `total`. Without
quota-based discovery, that total is unavailable in managed cloud and quota-governed
environments. This KEP provides it. The redistributive optimizer that acts on it is a
follow-up KEP scoped to the optimizer: a fair-share mode that activates when available
GPUs = 0 and multiple models are simultaneously saturated.

---

## Drawbacks

- Introduces a second discovery backend that must be maintained alongside
  `K8sWithGpuOperator`. The risk is low: both implement the same `FullDiscovery`
  interface and the new backend is simpler (two API calls vs. three vendor-specific
  node list calls).
- When a VA is provided, `acceleratorName` must be set or decisions for that variant will
  be skipped. The failure mode is conservative (no over-allocation) but requires operator
  attention. A log warning makes this diagnosable. VA itself is optional and will be
  deprecated.

---

## Alternatives

### Alternative 1 — New `ConstraintProvider` instead of `CapacityDiscovery`

Implement a standalone `QuotaConstraintProvider` that implements `ConstraintProvider`
directly (bypassing `TypeInventory`) and register it alongside the existing GPU limiter
in `engine.go`.

**Rejected because:** it duplicates the aggregation and pool-management logic already
in `TypeInventory`. The `CapacityDiscovery` interface exists precisely to separate the
"how do we learn about GPUs" concern from the "how do we track and allocate them" concern.
Reusing that separation is the right call.

### Alternative 2 — Pod-request summation for `DiscoverUsage()`

List all pods in the namespace and sum their GPU resource requests, mirroring
`K8sWithGpuOperator.DiscoverUsage()`.

**Rejected because:** `status.used` is the natural source of truth when quota is the
capacity model — it accounts for all admitted workloads, not just scheduled WVA-managed
pods, which is the correct scope when enforcing a namespace quota ceiling.

### Alternative 3 — Single `WVA_CAPACITY_NAMESPACE` (rejected)

Accept a single namespace name rather than a comma-separated list.

**Rejected because:** WVA already manages models across multiple namespaces. A single-
namespace restriction would make the feature unusable in any realistic multi-tenant
deployment. The comma-separated approach is no more complex to implement — the engine
loops over the list once at startup — and avoids a follow-up breaking change to add
multi-namespace support later.

### Alternative 4 — Environment variable per GPU type

Allow `WVA_QUOTA_CAPACITY_NVIDIA=8`, `WVA_QUOTA_CAPACITY_AMD=4` etc. as static overrides
without reading the actual quota.

**Rejected because:** it duplicates configuration already managed by the quota and
creates a second source of truth that can drift. Reading the live quota is always fresh.
