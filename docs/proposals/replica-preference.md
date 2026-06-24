# Proposal: Replica Preference

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-06-24
**Last Updated:** 2026-06-24

## Goal

Allow operators to rank GPU variants of the same model by preference. The
preferred tier's HPA scales normally and unmodified. When its GPU quota is
exhausted, the plugin unlocks the fallback tier so its HPA can absorb the
overflow. When preferred capacity is reclaimed, the fallback tier is parked again.
The design supports any number of tiers (e.g. H100 → A100 → T4) — each tier
spills into the next only when the tier above it is fully saturated.

## Non-Goals

- Cost optimization — the existing `llm-d.ai/variant-cost` annotation handles
  cost-aware allocation; preference is orthogonal to cost.
- Cross-namespace preference — variants are compared within a single namespace only.

---

## Background

WVA manages HPAs and KEDA ScaledObjects that carry the `llm-d.ai/managed: "true"`
annotation. Variants of the same model are grouped by `llm-d.ai/model-id`. For a
model running on both H100s and A100s there are two Deployments and two HPAs — one
per accelerator tier — sharing the same `model-id`.

There is currently no mechanism to express "scale H100s first; use A100s only when
H100 quota is full."

---

## Design

### Core Idea

Each tier is assigned a rank via the `llm-d.ai/accelerator-preference` annotation.
**A lower rank number means higher preference** — rank `1` is the most preferred
tier (e.g. H100), rank `2` is the first fallback (e.g. A100), and so on. The
default rank when the annotation is absent is `100`.

The preferred tier's `ResourceQuota` is discovered automatically by matching
`llm-d.ai/model-id` and `llm-d.ai/accelerator-preference` labels on the
ResourceQuota to the corresponding HPA annotations. No extra pointer annotation is
needed on the HPA. The plugin reads that quota each tick.

- **Preferred tier quota has spare capacity** (`used < hard`): set fallback
  `maxReplicas = minReplicas`. The fallback HPA is parked. If the operator sets
  `minReplicas=0`, the fallback scales to zero completely.
- **Preferred tier quota is exhausted** (`used >= hard`): restore fallback
  `maxReplicas` to its operator-configured value. The fallback HPA now scales
  normally based on its own demand signal.

The preferred tier's HPA is never modified. For KEDA ScaledObjects the plugin
patches `spec.maxReplicaCount` instead of `spec.maxReplicas`, since patching the
KEDA-generated HPA would be reverted on KEDA's next reconcile.

### Control Flow

Both HPAs can use the same demand signal (e.g. EPP queue depth). The plugin acts
as the gatekeeper by **patching `spec.maxReplicas` on the fallback HPA** each
tick. Kubernetes enforces `spec.maxReplicas` as an absolute ceiling — the HPA
controller will never scale beyond it regardless of what its metrics say.

The plugin patches `spec.maxReplicas`, not `spec.minReplicas`. `minReplicas` is
the floor and cannot prevent scale-up — the HPA controller would still scale the
fallback up when demand is high. `maxReplicas` is the hard ceiling the HPA
controller cannot exceed regardless of demand, making it the correct knob for
gating scale-up.

- **Parked**: plugin patches fallback `spec.maxReplicas = spec.minReplicas`. The
  HPA controller sees no headroom and cannot scale up. When `minReplicas=0` the
  deployment scales to zero.
- **Unlocked**: plugin patches fallback `spec.maxReplicas = manifestMax`. The HPA
  controller now has headroom and scales normally on its demand signal.

`manifestMax` is the operator's original `maxReplicas` value for the fallback HPA,
captured by the plugin before it first parks the fallback. It is written once to
the annotation `llm-d.ai/preference-manifest-max` on the fallback HPA, persisted
in etcd, and survives manager restarts. The plugin reads this annotation on every
unlock to know what ceiling to restore. If the operator later raises `maxReplicas`
above the stored value, the plugin detects `currentMax > manifestMax` on the next
tick and adopts the new value. Operators should not set this annotation manually —
the plugin owns it.

```
Demand rises → EPP queue depth increases
  ↓
H100 HPA sees queue > threshold → scales H100 freely      (plugin never touches it)
A100 HPA sees queue > threshold → cannot scale             (plugin has set maxReplicas=minReplicas)
  ↓
H100 quota exhausted: used == hard, no more H100 pods can be scheduled
  ↓
Plugin patches A100 spec.maxReplicas = manifestMax
  ↓
A100 HPA controller sees headroom → scales A100 on the same demand signal
```

### Capacity Derivation

```
spareGPUs = quota.spec.hard[gpuResource] - quota.status.used[gpuResource]
```

`gpuResource` is inferred from the preferred tier's scale target pod spec using
the vendor detection logic in `internal/utils/utils.go` (e.g.
`requests.nvidia.com/gpu`).

The plugin lists ResourceQuotas in the namespace filtered by:

```
llm-d.ai/model-id=<model-id>, llm-d.ai/accelerator-preference=<rank>
```

The list is performed on every tick with no local caching, so any quota change
made by an admin is reflected within one coordinator interval (default 15s).

If no matching ResourceQuota is found, the model group is skipped that tick and a
`Warning` is logged. If multiple ResourceQuotas match the same model-id and rank,
the plugin logs a `Warning` and skips the model group — operators must ensure at
most one ResourceQuota per tier. If the ResourceQuota exists but does not contain
the GPU resource key, `hard` and `used` both resolve to zero, spare = 0, and the
fallback is unlocked. If `hard = 0`, the plugin logs a `Warning` (likely operator
misconfiguration) and unlocks the fallback.

### Algorithm

```
For each (namespace, model-id) group:
  1. Sort variants by rank ascending (rank 1 = most preferred, rank 2 = first fallback, ...).
  2. If fewer than 2 distinct ranks exist, skip — nothing to park or unlock.
  3. For each tier with rank > 1, check only the next higher-preference tier (rank - 1):
       a. Read spareGPUs from that tier's ResourceQuota.
          On error (quota not found, API failure): skip this tier, log Warning.
       b. If spareGPUs > 0 (higher-preference tier has capacity):
            park   → setMaxReplicas(this tier, this tier's minReplicas)
       c. Else (higher-preference tier quota is exhausted):
            unlock → setMaxReplicas(this tier, this tier's manifestMax)
  4. Skip the patch when newMax == currentMax.
  5. Ceiling guard: never set maxReplicas > manifestMax.
```

Each tier only checks the tier immediately above it in preference, giving correct
waterfall behavior: A100 (rank 2) unlocks when H100 (rank 1) is exhausted; T4
(rank 3) unlocks when A100 (rank 2) is exhausted — not when H100 is exhausted
directly, since A100 absorbs the overflow first. Groups with only one tier or all
variants at the same rank are no-ops.

See the Control Flow section for the full `manifestMax` lifecycle.

---

## API

### Annotations

| Annotation | Set by | Required on | Description |
|---|---|---|---|
| `llm-d.ai/accelerator-preference` | operator | all tiers | Rank; lower = more preferred. Default `100`. |
| `llm-d.ai/preference-manifest-max` | plugin | fallback tiers | Operator-intended `maxReplicas`. Do not set manually. |

### ResourceQuota Labels

ResourceQuotas are matched to their tier by labels:

| Label | Value | Description |
|---|---|---|
| `llm-d.ai/model-id` | e.g. `ibm/granite-3b` | Matches the HPA's `llm-d.ai/model-id` annotation. |
| `llm-d.ai/accelerator-preference` | e.g. `"1"` | Matches the preferred tier's rank. |

### Example

```yaml
# GPU quota for the H100 tier — labels tie it to the H100 HPA
apiVersion: v1
kind: ResourceQuota
metadata:
  name: granite-h100-quota
  namespace: inference
  labels:
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "1"   # rank 1 = most preferred
spec:
  hard:
    requests.nvidia.com/gpu: "8"
  # scopeSelector is optional. When present it restricts the quota to pods
  # matching a specific PriorityClass, which is the recommended way to separate
  # H100 and A100 capacity budgets. Omit it to apply the quota to all pods in
  # the namespace.
  scopeSelector:
    matchExpressions:
      - operator: In
        scopeName: PriorityClass
        values: ["h100-inference"]
---
# GPU quota for the A100 tier — labels tie it to the A100 HPA
apiVersion: v1
kind: ResourceQuota
metadata:
  name: granite-a100-quota
  namespace: inference
  labels:
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "2"   # rank 2 = first fallback
spec:
  hard:
    requests.nvidia.com/gpu: "4"
  scopeSelector:
    matchExpressions:
      - operator: In
        scopeName: PriorityClass
        values: ["a100-inference"]
---
# H100 HPA — preferred tier; plugin never modifies this
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-h100
  namespace: inference
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "1"   # rank 1 = most preferred
spec:
  minReplicas: 2
  maxReplicas: 8
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: granite-h100
  metrics: [...]
---
# A100 HPA — fallback tier; plugin parks/unlocks this based on H100 quota
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-a100
  namespace: inference
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "2"   # rank 2 = first fallback
spec:
  minReplicas: 0
  maxReplicas: 4
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: granite-a100
  metrics: [...]
```

With light load, H100 quota has spare → plugin sets `granite-a100`
`spec.maxReplicas=0` (equal to `minReplicas`) → deployment scales to zero. When
H100 quota is exhausted, the plugin restores `spec.maxReplicas=4` and the A100
HPA scales normally on its demand signal.

---

## Implementation

### Phase 1 — Annotation Schema

**File:** `internal/annotations/annotations.go`

```go
// AcceleratorPreference ranks variants of the same model by preference.
// Lower value = higher preference. Defaults to 100 when absent.
AcceleratorPreference = "llm-d.ai/accelerator-preference"

// PreferenceManifestMax stores the operator-intended maxReplicas for fallback
// tiers. Written by the plugin on first observation. Do not set manually.
PreferenceManifestMax = "llm-d.ai/preference-manifest-max"

const defaultAcceleratorPreference = 100
```

### Phase 2 — Coordinator Plugin

**New package:** `internal/coordinator/plugins/preference/`

```
plugin.go     — Plugin struct, Name(), Tick() (groups objects by model-id inline)
algorithm.go  — resolveTiers(), spareGPUs(), manifestMax()
```

#### Plugin struct

```go
type Plugin struct {
    client client.Client
}
```

No Prometheus client required. No cooldown logic needed — HPA stabilization
windows (`spec.behavior.scaleUp/scaleDown.stabilizationWindowSeconds`) already
prevent oscillation on the HPA side. The plugin simply patches `maxReplicas`
whenever quota state changes.

#### spareGPUs

```go
func (p *Plugin) spareGPUs(ctx context.Context, ns, modelID string, rank int) (int64, error) {
    quotaList := &corev1.ResourceQuotaList{}
    if err := p.client.List(ctx, quotaList,
        client.InNamespace(ns),
        client.MatchingLabels{
            annotations.ModelID:              modelID,
            annotations.AcceleratorPreference: strconv.Itoa(rank),
        },
    ); err != nil {
        return 0, fmt.Errorf("listing ResourceQuotas for model %s rank %d: %w", modelID, rank, err)
    }
    if len(quotaList.Items) == 0 {
        return 0, fmt.Errorf("no ResourceQuota found for model %s rank %d in namespace %s", modelID, rank, ns)
    }
    quota := &quotaList.Items[0]
    gpuKey := gpuResourceKey(ns, modelID, p.client, ctx) // inferred via utils.GetProductKeys()
    hard := quota.Spec.Hard[corev1.ResourceName(gpuKey)]
    used := quota.Status.Used[corev1.ResourceName(gpuKey)]
    return hard.Value() - used.Value(), nil
}
```

#### RBAC markers

```go
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;patch;update
```

### Phase 3 — Feature Flag

**Files:** `internal/config/loader.go`, `internal/config/config.go`

Add the flag following the existing viper pattern:

```go
// internal/config/loader.go
v.SetDefault("PREFERENCE_REBALANCE_ENABLED", false)

// internal/config/config.go
func (c *Config) PreferenceEnabled() bool {
    return c.v.GetBool("PREFERENCE_REBALANCE_ENABLED")
}
```

Default is `false`. When `false`, the plugin's `Tick` returns immediately without
reading or writing anything.

### Phase 4 — Wire Into Coordinator

**File:** `cmd/main.go`

Inside the existing `cfg.CoordinatorEnabled()` block, conditionally append the
preference plugin:

```go
plugins := []coordinator.Plugin{
    gpurebalance.New(mgr.GetClient(), promAPI),
}
if cfg.PreferenceEnabled() {
    plugins = append(plugins, preference.New(mgr.GetClient()))
}
```

### Phase 5 — Observability

| Metric | Type | Labels | Description |
|---|---|---|---|
| `wva_preference_parked_total` | Counter | `namespace`, `model`, `hpa` | Times a fallback HPA was parked |
| `wva_preference_unlocked_total` | Counter | `namespace`, `model`, `hpa` | Times a fallback HPA was unlocked |
| `wva_preference_spare_gpus` | Gauge | `namespace`, `model`, `rank` | Spare GPU units in the tier's quota |

Every park/unlock logs at `Info` level:

```
action=park|unlock  hpa=<name>  namespace=<ns>  model=<model-id>
from=<old>  to=<new>  spare_gpus=<n>  rank=<n>
```

---

## Rollout

1. Create a `ResourceQuota` scoped to the preferred tier's pods with labels
   `llm-d.ai/model-id` and `llm-d.ai/accelerator-preference` matching the
   preferred HPA's annotations.
2. Verify exactly one ResourceQuota exists per tier per model — duplicate labels
   cause the plugin to skip that model group with a warning:
   ```
   kubectl get resourcequotas -n <ns> -l llm-d.ai/model-id=<model-id>
   ```
3. Add `llm-d.ai/accelerator-preference` to all HPAs for the model. No changes
   needed to the fallback's Deployment or quota.
4. Deploy with `PREFERENCE_REBALANCE_ENABLED: "false"` in the manager ConfigMap.
   No behavioral change; the plugin is registered but its `Tick` exits immediately.
5. Set `PREFERENCE_REBALANCE_ENABLED: "true"` in the manager ConfigMap and restart
   the manager pod.
6. On the first tick the plugin writes `llm-d.ai/preference-manifest-max` to
   fallback HPAs. Verify the annotation matches the intended `maxReplicas`.
7. Observe `wva_preference_spare_gpus` to confirm quota is being read correctly.
8. Run load test: verify fallback stays parked under light load, unlocks when H100
   quota is exhausted, and parks again when load subsides.

No CRD migrations, no schema version bumps, no downtime required.

---

## Testing Plan

### Unit Tests (`make test`)

- `annotations_test.go`: `AcceleratorPreference` — absent (→ 100), zero (→ error),
  negative (→ error), valid.
- `algorithm_test.go`: single tier (no-op), equal ranks (no-op), preferred spare > 0
  (fallback parked), preferred spare = 0 (fallback unlocked), floor guard, ceiling
  guard.
- `spareGPUs` tests: quota found with spare, quota fully consumed, GPU key absent
  in quota (spare = 0), no matching quota (→ error).

### Integration Tests (`make test`)

- Preferred quota has spare → assert fallback `maxReplicas` set to `minReplicas`.
- Preferred quota exhausted → assert fallback `maxReplicas` restored to `manifestMax`.
- No ResourceQuota matching preferred tier labels → assert no patch, warning logged.
- Manifest persistence: park fallback, restart plugin, assert unlock restores the
  correct `manifestMax` from annotation.
- KEDA path: fallback is a ScaledObject → assert `spec.maxReplicaCount` is patched.

### E2E Tests (`make test-e2e-smoke`)

1. Light load: H100 quota has spare → A100-sim stays at `minReplicas`.
2. Ramp load until H100 quota is exhausted → A100-sim is unlocked and scales up.
3. Load subsides → H100 quota recovers → A100-sim is parked again.

All images must use fully-qualified registry paths (`registry.k8s.io/`, `quay.io/`,
or a private registry). Docker Hub is not permitted.

