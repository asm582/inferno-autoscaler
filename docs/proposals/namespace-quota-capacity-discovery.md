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
- [Deprecation Notice: VariantAutoscaling](#deprecation-notice-variantautoscaling)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)

---

## Summary

Add `NamespaceQuotaDiscovery`, a new implementation of the existing `CapacityDiscovery`
interface that derives GPU capacity from Kubernetes `ResourceQuota` objects rather than
from GPU Operator node labels. When enabled via `WVA_CAPACITY_NAMESPACES=ns-a,ns-b,ns-c`,
the engine replaces `e.GPULimiter` with a `NamespaceLimiter` that holds one quota-backed
`DefaultLimiter` per namespace. Both the V1 (`optimizeV1`) and V2 (`optimizeV2`) paths
consume `e.GPULimiter` and therefore enforce per-namespace quota ceilings with no
changes to the optimizers or `TypeInventory`. `DefaultLimiter` gains a
`useDiscoveredUsage` flag and a new constructor to read usage from
`ResourceQuota.status.used` via `RefreshAll()` instead of computing it from decision
replicas.

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

In each of these cases a namespace-scoped quota is a practical fallback: the quota hard
limit equals the GPU capacity visible to workloads in the namespace. Where GPU Operator
labels are accessible, `K8sWithGpuOperator` remains the preferred backend — it provides
GPU-model-aware scheduling and more precise per-node inventory that quota cannot express.

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

1. The autoscaler enforces GPU capacity limits derived from `ResourceQuota` in
   environments where GPU Operator labels are unavailable.
2. Multiple namespaces each have their own independently enforced GPU quota ceiling —
   saturation in one namespace does not affect headroom in another.
3. Works on any Kubernetes distribution — GKE, EKS, AKS, OpenShift, kind — without
   requiring GPU Operator or node-level RBAC.

### Non-Goals

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
> change replica count beyond what the cluster can place.

**Story 2 — Quota-first operations**

> As a platform team that manages GPU capacity via quota rather than node labels,
> I want a single source of truth: the quota.

### Notes and Constraints

- **Quota equals capacity.** This design treats `ResourceQuota.spec.hard` as the total
  GPU capacity available to workloads in the namespace. No node inventory, no GPU Operator
  labels — the quota is the single source of truth for how many GPUs can be scheduled.
  Keeping the quota aligned with actual node capacity is the operator's responsibility.

- **Accelerator key is the resource name, not the model name.** `K8sWithGpuOperator`
  derives pool keys from GPU node labels (e.g. `NVIDIA-A100-PCIE-80GB`). `ResourceQuota`
  carries no model information — it stores only the Kubernetes resource name:

  ```yaml
  spec:
    hard:
      nvidia.com/gpu: 8   # ← this string becomes the TypeInventory pool key
  ```

  `TypeInventory` pools are therefore keyed by `nvidia.com/gpu` (or `amd.com/gpu`,
  `intel.com/gpu`). All GPUs in a namespace are treated as one undifferentiated pool.
  Differentiating by model (A100 vs H100) within a namespace is not supported — use
  `K8sWithGpuOperator` for heterogeneous GPU namespaces.

- **`VariantAutoscaling` is optional and deprecated.** VA is the older mechanism for
  attaching per-variant scaling configuration (`gpusPerReplica`, `acceleratorName`, etc.)
  to a model variant. It is not required and will be removed in a future release (see
  [Deprecation Notice](#deprecation-notice-variantautoscaling)).

  When a VA is provided, `acceleratorName` must match the `TypeInventory` pool key for
  the limiter to apply the correct GPU budget to that variant's decisions. In quota mode
  the pool key is the resource name — `nvidia.com/gpu` — not a GPU model name like `A100`.
  A mismatch between `acceleratorName` and the pool key causes the variant's decisions to
  be skipped entirely (conservative: no over-allocation, but no scaling either).

  In practice, operators do not need to set `acceleratorName` at all for homogeneous GPU
  namespaces. `resolveUnknownAccelerators()` auto-resolves an empty or unrecognised value
  to the single pool key when the namespace inventory has exactly one GPU pool. Operators
  switching from `K8sWithGpuOperator` must clear any stale model-specific names (e.g.
  `A100`) that no longer match the quota pool key.

- **V1 usage covers all namespace workloads.** V1 reads `ResourceQuota.status.used`
  via `TypeInventory.RefreshAll()`, reflecting all GPU consumers in the namespace.
  Available GPUs = `spec.hard` − `status.used`. V2 currently derives usage from
  WVA-managed decisions only; non-WVA workloads are not counted in V2 constraints
  (see recommendation in the Design Details section).

- **Each namespace's quota is enforced independently.** Each namespace listed in
  `WVA_CAPACITY_NAMESPACES` gets its own `DefaultLimiter` backed by its own
  `NamespaceQuotaDiscovery`. `NamespaceLimiter` ensures that scaling decisions for ns-a
  are only evaluated against ns-a's `ResourceQuota`, and ns-b's against ns-b's. A saturated model in ns-a consuming its full quota does not reduce the
  GPU headroom available to models in ns-b.



### Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `status.used` transiently stale during rapid pod churn | Low | Low | The quota admission controller updates `status.used` synchronously on pod admission. Staleness is bounded by API server response time, well within the 30-second optimization interval. |
| Operator leaves `acceleratorName` set to `A100` after switching from `K8sWithGpuOperator` | Low | Low | `resolveUnknownAccelerators()` auto-resolves empty/unknown names to `nvidia.com/gpu` when the namespace has a single GPU pool. Operators must clear stale model-specific names. Logged as a warning. |
| Mixed GPU types in one namespace share one pool | Low | Low | Documented as a known limitation. Use `K8sWithGpuOperator` for heterogeneous GPU namespaces. |

---

## Design Details

### Overview

Two new files, targeted edits to four existing files. No new exported Go interfaces,
no new fields on `Engine`, no changes to `optimizeV1`, `TypeInventory`, or any optimizer.
`DefaultLimiter` gains a `useDiscoveredUsage` flag and a new constructor.

```
internal/discovery/
  namespace_quota.go           # NEW — NamespaceQuotaDiscovery
  namespace_quota_test.go      # NEW

internal/engines/pipeline/
  namespace_limiter.go         # NEW — NamespaceLimiter wrapper
  namespace_limiter_test.go    # NEW
  default_limiter.go           # EDIT — useDiscoveredUsage flag, ~20 lines
  type_inventory.go            # EDIT — narrow NewTypeInventoryWithUsage parameter, 1 line

internal/engines/saturation/
  engine.go                    # EDIT — NewEngine() only, ~15 lines
```

---

### `internal/discovery/namespace_quota.go`

`NamespaceQuotaDiscovery` implements `CapacityDiscovery` and `UsageDiscovery` by reading
`ResourceQuota` objects. It does not implement `NodeDiscovery` — quota objects carry no
per-node information, so there is nothing to return. `NewTypeInventoryWithUsage` is
narrowed (see below) to accept only the two interfaces it actually uses, so no stub
`DiscoverNodes` method is needed.

**`Discover(ctx) → map[quotaName]map[resourceName]AcceleratorModelInfo`**

1. List `ResourceQuotaList` in `d.Namespace`.
2. For each quota, scan `Spec.Hard` for GPU resource names via `gpuResourceName()`.
3. Return each quota as its own outer key so `TypeInventory.Refresh()` aggregates
   multiple quotas in the same namespace without modification.

**`DiscoverUsage(ctx) → map[resourceName]int`**

Lists `ResourceQuotaList` and sums `Status.Used` values per canonical GPU resource.
Called by both V1 and V2 via `TypeInventory.RefreshAll()` — see `default_limiter.go`
changes below.

**`gpuResourceName(res string) string`**

Internal helper. Strips the `requests.` prefix then matches known GPU resource names.
This prevents `requests.nvidia.com/gpu` and `nvidia.com/gpu` from creating two separate
pool keys and double-counting when both appear in one quota.

```go
func gpuResourceName(res string) string {
    res = strings.TrimPrefix(res, "requests.")
    switch res {
    case "nvidia.com/gpu", "amd.com/gpu", "intel.com/gpu":
        return res
    }
    return ""
}
```

| Quota resource key | Canonical pool key |
|---|---|
| `nvidia.com/gpu` | `nvidia.com/gpu` |
| `requests.nvidia.com/gpu` | `nvidia.com/gpu` (prefix stripped) |
| `amd.com/gpu` | `amd.com/gpu` |
| `requests.amd.com/gpu` | `amd.com/gpu` (prefix stripped) |
| `intel.com/gpu` | `intel.com/gpu` |
| `requests.intel.com/gpu` | `intel.com/gpu` (prefix stripped) |

```go
var _ discovery.CapacityDiscovery = (*NamespaceQuotaDiscovery)(nil)
var _ discovery.UsageDiscovery    = (*NamespaceQuotaDiscovery)(nil)
```

---

### `internal/engines/pipeline/namespace_limiter.go`

`NamespaceLimiter` wraps a map of per-namespace `DefaultLimiter`s and implements the
existing `Limiter` interface. It is the single integration point for both V1 and V2.

```go
type NamespaceLimiter struct {
    limiters map[string]*DefaultLimiter // keyed by namespace
}

func NewNamespaceLimiter(limiters map[string]*DefaultLimiter) *NamespaceLimiter

func (n *NamespaceLimiter) Name() string { return "namespace-quota-limiter" }

// Limit satisfies the Limiter interface — used by V1 (optimizeV1).
// Groups decisions by d.Namespace and calls the matching DefaultLimiter.Limit().
// Namespaces not in the map run unlimited.
func (n *NamespaceLimiter) Limit(ctx context.Context, decisions []*interfaces.VariantDecision) error

// LimiterForNamespace returns the DefaultLimiter for ns, or nil if not quota-governed.
// Used by the V2 path to call ComputeConstraints() per namespace.
func (n *NamespaceLimiter) LimiterForNamespace(ns string) *DefaultLimiter

var _ Limiter = (*NamespaceLimiter)(nil)
```

---

### `internal/engines/pipeline/type_inventory.go` — narrow constructor parameter

`NewTypeInventoryWithUsage` currently accepts `discovery.FullDiscovery`, but `TypeInventory`
only stores and calls `CapacityDiscovery` and `UsageDiscovery` — it never calls
`DiscoverNodes`. The parameter is narrowed to a consumer-side interface defined locally
in the `pipeline` package:

```go
// quotaInventorySource is the narrow contract NewTypeInventoryWithUsage actually needs.
// Defined here (consumer side) so the discovery package does not dictate the shape.
type quotaInventorySource interface {
    discovery.CapacityDiscovery
    discovery.UsageDiscovery
}

func NewTypeInventoryWithUsage(name string, disc quotaInventorySource) *TypeInventory {
```

Both `K8sWithGpuOperator` and `NamespaceQuotaDiscovery` satisfy `quotaInventorySource`
with no changes — they both implement `Discover` and `DiscoverUsage`. The single existing
call site in `engine.go` passes a `*K8sWithGpuOperator`, which still compiles unchanged.

---

### `internal/engines/pipeline/default_limiter.go` — `useDiscoveredUsage` flag

Add a `useDiscoveredUsage` flag and a new constructor. When set, `Limit()` and
`ComputeConstraints()` call `RefreshAll()` instead of `Refresh()` +
`calculateUsedGPUs()`/`SetUsed()`. `RefreshAll()` calls both `Discover()` (reads
`spec.hard`) and `DiscoverUsage()` (reads `status.used`) in one pass — covering all
GPU workloads in the namespace, not just WVA-managed ones.

Define a consumer-side interface at the top of `default_limiter.go` for the narrow
behavior `DefaultLimiter` needs in the `useDiscoveredUsage` path:

```go
// fullRefreshInventory is the narrow contract required by the useDiscoveredUsage path.
// Defined on the consumer side so DefaultLimiter is not coupled to *TypeInventory.
type fullRefreshInventory interface {
    Inventory
    RefreshAll(ctx context.Context) error
}
```

`DefaultLimiter` gains a dedicated `refresher` field (set only when
`useDiscoveredUsage=true`) so `Limit()` and `ComputeConstraints()` call the interface
directly — no type assertions anywhere:

```go
type DefaultLimiter struct {
    name               string
    inventory          Inventory
    refresher          fullRefreshInventory // non-nil iff useDiscoveredUsage; same value as inventory
    algorithm          AllocationAlgorithm
    metricsEmitter     *metrics.MetricsEmitter
    useDiscoveredUsage bool
}
```

`NewDefaultLimiterWithDiscoveredUsage` accepts `fullRefreshInventory`, satisfying the
contract at compile time and storing it in both fields:

```go
// NewDefaultLimiterWithDiscoveredUsage creates a limiter that reads usage from
// DiscoverUsage() (ResourceQuota.status.used) instead of computing it from
// decision replicas. Use for quota-backed inventories.
func NewDefaultLimiterWithDiscoveredUsage(name string, inventory fullRefreshInventory, algorithm AllocationAlgorithm) *DefaultLimiter {
    return &DefaultLimiter{
        name:               name,
        inventory:          inventory,
        refresher:          inventory,
        algorithm:          algorithm,
        metricsEmitter:     metrics.NewMetricsEmitter(),
        useDiscoveredUsage: true,
    }
}
```

`TypeInventory` satisfies `fullRefreshInventory` — it implements `Inventory` and has
`RefreshAll`. The existing `NewDefaultLimiter` constructor leaves `refresher` nil and
`useDiscoveredUsage` false; it is unchanged.

**`Limit()` — replace the first two steps:**

```go
if l.useDiscoveredUsage {
    // reads spec.hard (capacity) + status.used (usage) in one call
    if err := l.refresher.RefreshAll(ctx); err != nil {
        return fmt.Errorf("failed to refresh quota inventory: %w", err)
    }
} else {
    if err := l.inventory.Refresh(ctx); err != nil {
        return fmt.Errorf("failed to refresh inventory: %w", err)
    }
    l.inventory.SetUsed(l.calculateUsedGPUs(decisions))
}
```

**`ComputeConstraints()` — same pattern:**

```go
if l.useDiscoveredUsage {
    if err := l.refresher.RefreshAll(ctx); err != nil {
        return nil, fmt.Errorf("failed to refresh quota inventory: %w", err)
    }
} else {
    if err := l.inventory.Refresh(ctx); err != nil {
        return nil, fmt.Errorf("failed to refresh inventory: %w", err)
    }
    l.inventory.SetUsed(currentUsage)
}
```

When `useDiscoveredUsage` is true the `currentUsage` parameter to `ComputeConstraints()`
is ignored — usage comes from `status.used` via `RefreshAll()`.

---

### `engine.go` — `NewEngine()` change

After the existing `gpuLimiter` is built, override it with a `NamespaceLimiter` when
`WVA_CAPACITY_NAMESPACES` is set. The `GPULimiter: gpuLimiter` assignment is unchanged.

```go
// existing lines — unchanged
gpuDiscovery := discovery.NewK8sWithGpuOperator(client)
gpuInventory := pipeline.NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
gpuLimiter   := pipeline.NewDefaultLimiter("gpu-limiter", gpuInventory, pipeline.NewGreedyBySaturation())

// NEW: replace with NamespaceLimiter when quota mode is configured
if nsEnv := os.Getenv("WVA_CAPACITY_NAMESPACES"); nsEnv != "" {
    perNS := make(map[string]*pipeline.DefaultLimiter)
    for _, ns := range strings.Split(nsEnv, ",") {
        ns = strings.TrimSpace(ns)
        if ns == "" {
            continue
        }
        disc := discovery.NewNamespaceQuotaDiscovery(client, ns)
        inv  := pipeline.NewTypeInventoryWithUsage("quota-inventory-"+ns, disc)
        perNS[ns] = pipeline.NewDefaultLimiterWithDiscoveredUsage("quota-limiter-"+ns, inv, pipeline.NewGreedyBySaturation())
    }
    gpuLimiter = pipeline.NewNamespaceLimiter(perNS)
}

// existing line — unchanged
GPULimiter: gpuLimiter,
```

**V1 — reads both `spec.hard` and `status.used` from the quota.** `optimizeV1` calls
`e.GPULimiter.Limit(ctx, decisionPtrs)`. With `e.GPULimiter` set to a `NamespaceLimiter`,
`Limit()` groups decisions by `d.Namespace` and calls each namespace's
`DefaultLimiter.Limit()`. Because the per-namespace limiters are created with
`NewDefaultLimiterWithDiscoveredUsage()`, `Limit()` calls `TypeInventory.RefreshAll()`
which reads `ResourceQuota.spec.hard` (capacity) and `ResourceQuota.status.used` (usage)
in one pass. Zero changes to `optimizeV1` itself.

**V2 already reads `spec.hard` from the quota.** `ComputeConstraints()` calls
`inventory.Refresh(ctx)` → `NamespaceQuotaDiscovery.Discover()` — reading
`ResourceQuota.spec.hard`. The V2 change below is needed only to dispatch per-namespace
rather than treating all requests as a single global pool.

> **Recommendation:** For consistency with V1, V2 should also adopt
> `NewDefaultLimiterWithDiscoveredUsage()` so `ComputeConstraints()` reads
> `ResourceQuota.status.used` via `RefreshAll()` instead of using
> `computeCurrentGPUUsage(nsRequests)`. This ensures non-WVA GPU workloads are
> accounted for in V2 constraints. Tracked as a follow-up.

---

### API Changes

No new CRDs, no new API types, no new flags in the saturation ConfigMap, no new Go
interfaces. The only user-facing surface is the `WVA_CAPACITY_NAMESPACES` environment
variable and the `acceleratorName` convention on VA specs.

---

### Accelerator Name Convention

| Discovery mode | `TypeInventory` pool key | VA `acceleratorName` |
|---|---|---|
| `K8sWithGpuOperator` | `A100`, `H100` (normalized from node label) | `A100` |
| `NamespaceQuotaDiscovery` | `nvidia.com/gpu` (canonical resource name) | `nvidia.com/gpu` or empty (auto-resolved) |

`DefaultLimiter.resolveUnknownAccelerators()` auto-resolves an empty or unrecognised
`acceleratorName` to `nvidia.com/gpu` when the namespace inventory has exactly one pool
key. Operators do not need to set `acceleratorName` for homogeneous GPU namespaces.
Operators switching from `K8sWithGpuOperator` must clear any stale model-specific names
(e.g. `A100`) that no longer match the quota pool key.

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
| No GPU entries in quota | Only CPU and memory limits | Empty map, no error |
| `requests.nvidia.com/gpu` prefix | Prefixed hard limit: 4 | Counted as `nvidia.com/gpu: 4` |
| Multi-vendor quota | `nvidia.com/gpu: 4`, `amd.com/gpu: 2` | Both keys present with correct counts |
| Empty namespace | No `ResourceQuota` objects | Empty map, no error |

#### `DiscoverUsage()` coverage

| Test | Input | Expected |
|---|---|---|
| GPUs in use | `status.used["nvidia.com/gpu"]: 3` | `{"nvidia.com/gpu": 3}` |
| No usage | `status.used` empty or absent | Empty map, no error |
| Multi-vendor usage | Both `nvidia.com/gpu` and `amd.com/gpu` used | Both keys returned |

### Integration Tests

Integration tests validate the full pipeline in-process using a fake client. No cluster
required.

**Pipeline — discovery → inventory → `DefaultLimiter.Limit()` (V1 path):**

```bash
go test ./internal/discovery/... ./internal/engines/pipeline/... -run TestQuotaLimiter
```

| Test | What it validates |
|---|---|
| Hard limit enforced | `spec.hard["nvidia.com/gpu"]: 4`; two VAs requesting 3 replicas each → `TargetReplicas` capped so total GPUs ≤ 4 |
| Usage read from `status.used` | `status.used["nvidia.com/gpu"]: 3`; available = 1 → only 1 GPU worth of scale-up permitted |
| Zero hard limit | `spec.hard["nvidia.com/gpu"]: 0` → no scale-up decisions produced |
| Usage at limit | `status.used` equals `spec.hard` → `TargetReplicas` unchanged for all decisions |
| Two namespaces independent | ns-a quota: 4 GPUs, ns-b quota: 8 GPUs; `NamespaceLimiter.Limit()` caps each namespace independently |
| Unknown namespace runs unlimited | Decision in ns-c (not in limiter map) passes through unconstrained |
| Limiter error logged, continues | One namespace's `DefaultLimiter` returns error; other namespaces are still limited |
| Quota discovery error | `Discover()` returns error → `Limit()` propagates it, namespace runs unlimited |

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

### Migration Path

**Step 1 — Create `ResourceQuota` in each namespace** (if not already present):

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: gpu-quota
  namespace: <ns>
spec:
  hard:
    nvidia.com/gpu: "<N>"
```

**Step 2 — Set `WVA_CAPACITY_NAMESPACES`** on the controller Deployment and restart:

```yaml
env:
- name: WVA_CAPACITY_NAMESPACES
  value: "ns-a,ns-b"
```

**Step 3 — Verify** the controller logs show `"quota-based capacity discovered"` with the
correct `namespace` and `totalGPUs` fields.

**Rollback:** unset `WVA_CAPACITY_NAMESPACES` and restart. The controller reverts to
`K8sWithGpuOperator` immediately with no state to clean up.

> **Note on `acceleratorName` label:** Do not clear the
> `inference.optimization/acceleratorName` label on VAs during migration. In quota-mode
> environments (managed cloud, no GPU Operator), `GetAcceleratorNameFromScaleTarget()`
> finds no GPU product labels on nodes and returns `"unknown"`, which
> `resolveUnknownAccelerators()` maps to `nvidia.com/gpu` automatically. Clearing the
> label on clusters migrating from `K8sWithGpuOperator` would break rollback on
> heterogeneous clusters (A100 + H100) where the label is needed for GPU-model-aware
> pool selection.

---

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

- `wva_available_gpus` is populated from whichever `TypeInventory` backs `e.GPULimiter`.
  In quota mode `e.GPULimiter` is a `NamespaceLimiter` wrapping per-namespace
  `DefaultLimiter`s; the metric will reflect the quota-backed inventory for each
  namespace that the limiter processes.
- Log line at `Info` level on every `Discover()` call: `"quota-based capacity discovered"`
  with `namespace`, `totalGPUs`, and `quotaCount` fields.
- Log line at `Warning` level when `Discover()` returns an empty map for a configured
  namespace (likely missing or misconfigured `ResourceQuota`).

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

## Deprecation Notice: VariantAutoscaling

`VariantAutoscaling` (VA) is deprecated. New deployments should not create VA objects.
Existing VA objects continue to function but will be removed in a future release.

**Why deprecated:** VA predates the current scaling architecture. Its fields
(`acceleratorName`, `gpusPerReplica`) are now expressed directly on the scale target
spec, making the separate VA object redundant.

**Impact in quota mode:** If you have existing VA objects and are switching to quota-based
discovery, audit `acceleratorName` on each VA. Any value that was valid under
`K8sWithGpuOperator` (e.g. `A100`, `H100`) will not match the quota pool key
(`nvidia.com/gpu`) and must be updated or cleared. An empty `acceleratorName` is safe for
homogeneous GPU namespaces — `resolveUnknownAccelerators()` auto-resolves it. A mismatch
is logged as a warning; the decision is skipped (no over-allocation, but no scaling).

**Migration:** Remove VA objects and configure accelerator settings on the scale target
directly. If VA removal is not immediately feasible, set `acceleratorName: nvidia.com/gpu`
(or the appropriate vendor resource name) before switching discovery backends.

---

## Drawbacks

- Introduces a second discovery backend that must be maintained alongside
  `K8sWithGpuOperator`. The risk is low: both implement the same `FullDiscovery`
  interface and the new backend is simpler (two API calls vs. three vendor-specific
  node list calls).
- When a VA is provided, `acceleratorName` must match the quota pool key or the
  variant's decisions will be skipped. The failure mode is conservative (no
  over-allocation) but requires operator attention. A log warning makes this diagnosable.

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
