# Proposal: Replica Preference

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-06-24
**Last Updated:** 2026-06-24

## Goal

Allow operators to rank variants of the same model by preference. The preferred
variant's HPA scales normally and unmodified. When its GPU quota is exhausted, the
plugin unlocks the fallback variant so its HPA can absorb the overflow. When
preferred capacity is reclaimed, the fallback variant is parked again. The design
supports any number of variants (e.g. H100 → A100 → T4) — each variant spills
into the next only when the variant above it is fully saturated.

## Non-Goals

- Cost optimization — the existing `llm-d.ai/variant-cost` annotation handles
  cost-aware allocation; preference is orthogonal to cost.
- Cross-namespace preference — variants are compared within a single namespace only.

---

## Background

WVA manages HPAs and KEDA ScaledObjects that carry the `llm-d.ai/managed: "true"`
annotation. In llm-d, an `InferencePool` is the unit that groups inference backends
serving the same model. A namespace can contain multiple InferencePools — one per
model. Each InferencePool can have multiple variants backed by different accelerators
— for example, an H100 Deployment and an A100 Deployment both serving the same pool.
Each variant has its own HPA, and all HPAs for the same model carry the
`llm-d.ai/model-id` annotation.

There is currently no mechanism to express "scale H100s first; use A100s only when
H100 quota is full."

---

## Design

### Core Idea

Variants of the same model are ranked via the `llm-d.ai/accelerator-preference`
annotation on their HPA. **A lower rank number means higher preference** — rank `1`
is the most preferred variant (e.g. H100), rank `2` is the first fallback (e.g.
A100), and so on. The default rank when the annotation is absent is `100`.

The plugin groups HPAs by `(namespace, llm-d.ai/model-id)` — the same grouping key
already used by the rest of WVA. Within each group, variants are ranked and the
plugin gates fallback variants based on the preferred variant's `ResourceQuota`. The
quota is discovered automatically by matching `llm-d.ai/model-id` and
`llm-d.ai/accelerator-preference` labels on the ResourceQuota to the corresponding
HPA annotations. No extra annotation is needed on the HPA. The plugin reads that
quota each tick.

> **Note:** This design assumes one InferencePool per model-id. A namespace can hold
> multiple models (each with its own `model-id` and its own InferencePool), and
> preference is applied independently per model.

- **Preferred variant quota has spare capacity** (`used < hard`): set fallback
  `maxReplicas = minReplicas`. The fallback HPA is parked. If the operator sets
  `minReplicas=0`, the fallback scales to zero completely.
- **Preferred variant quota is exhausted** (`used >= hard`): restore fallback
  `maxReplicas` to its operator-configured value. The fallback HPA now scales
  normally based on its own demand signal.

The preferred variant's HPA is never modified. For KEDA ScaledObjects the plugin
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

Each variant discovers its capacity via a `ResourceQuota` that the operator labels
to match that variant's HPA annotations:

| HPA annotation | must equal | ResourceQuota label |
|---|---|---|
| `llm-d.ai/model-id` | `=` | `llm-d.ai/model-id` |
| `llm-d.ai/accelerator-preference` | `=` | `llm-d.ai/accelerator-preference` |

The plugin reads the quota each tick using those two labels as a selector — no
pointer annotation on the HPA is needed. Spare capacity is:

```
spareGPUs = quota.spec.hard[gpuResource] - quota.status.used[gpuResource]
```

`gpuResource` is inferred from the variant's scale target pod spec via the vendor
detection logic in `internal/utils/utils.go` (e.g. `requests.nvidia.com/gpu`).

Every variant that can act as a gating signal needs a quota — in practice, every
variant except the lowest-rank one. In a two-variant setup only the rank-1
(H100) variant needs a quota. In a three-variant setup both rank-1 (H100) and
rank-2 (A100) variants need quotas; rank-3 (T4) does not, since nothing checks it
to gate another variant.

Quota changes made by an admin are reflected within one coordinator interval
(default 15s) — the list is performed on every tick with no local caching.

Error handling:

- No matching quota → skip the pool group that tick, log `Warning`.
- Multiple quotas match the same pool and rank → skip the pool group, log `Warning`. Operators must ensure exactly one quota per variant per pool.
- GPU resource key absent in quota → `hard=0, used=0, spare=0` → fallback unlocked.
- `hard=0` → log `Warning` (likely misconfiguration), fallback unlocked.

### Algorithm

```
For each (namespace, inference-pool) group:
  1. Sort variants by rank ascending (rank 1 = most preferred, rank 2 = first fallback, ...).
  2. If fewer than 2 distinct ranks exist, skip — nothing to park or unlock.
  3. For each variant with rank > 1, check only the next higher-preference variant (rank - 1):
       a. Read spareGPUs from that variant's ResourceQuota.
          On error (quota not found, API failure): skip this variant, log Warning.
       b. If spareGPUs > 0 (higher-preference variant has capacity):
            park   → setMaxReplicas(this variant, this variant's minReplicas)
       c. Else (higher-preference variant quota is exhausted):
            unlock → setMaxReplicas(this variant, this variant's manifestMax)
  4. Skip the patch when newMax == currentMax.
  5. Ceiling guard: never set maxReplicas > manifestMax.
```

Each variant only checks the variant immediately above it in preference, giving
correct waterfall behavior: A100 variant (rank 2) unlocks when H100 variant
(rank 1) is exhausted; T4 variant (rank 3) unlocks when A100 variant (rank 2) is
exhausted — not when H100 is exhausted directly, since A100 absorbs the overflow
first. Groups with only one variant or all variants at the same rank are no-ops.

See the Control Flow section for the full `manifestMax` lifecycle.

---

## API

### Annotations

| Annotation | Set by | Required on | Description |
|---|---|---|---|
| `llm-d.ai/model-id` | operator | all variants | Existing annotation identifying the model. Used as the grouping key. |
| `llm-d.ai/accelerator-preference` | operator | all variants | Rank within the model group; lower = more preferred. Default `100`. |
| `llm-d.ai/preference-manifest-max` | plugin | fallback variants | Operator-intended `maxReplicas`. Do not set manually. |

### ResourceQuota Labels

ResourceQuotas are matched to their variant by labels:

| Label | Value | Description |
|---|---|---|
| `llm-d.ai/model-id` | e.g. `ibm/granite-3b` | Matches the HPA's `llm-d.ai/model-id` annotation. |
| `llm-d.ai/accelerator-preference` | e.g. `"1"` | Matches the variant's rank within the model group. |

### Example

```yaml
# GPU quota for the H100 variant — labels tie it to the H100 HPA via model-id + rank
apiVersion: v1
kind: ResourceQuota
metadata:
  name: granite-3b-h100-quota
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
# H100 HPA — preferred variant (rank 1); plugin never modifies this
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-3b-h100
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
    name: granite-3b-h100
  metrics: [...]
---
# A100 HPA — fallback variant (rank 2); plugin parks/unlocks this based on H100 quota
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-3b-a100
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
    name: granite-3b-a100
  metrics: [...]
```

The A100 ResourceQuota is omitted here because in a two-variant setup nothing
checks it to gate another variant — only the H100 quota is needed.

With light load, H100 quota has spare → plugin sets `granite-a100`
`spec.maxReplicas=0` (equal to `minReplicas`) → deployment scales to zero. When
H100 quota is exhausted, the plugin restores `spec.maxReplicas=4` and the A100
HPA scales normally on its demand signal.

---

## Scenario: `ibm/granite-3b` with H100 and A100 variants

This section shows the complete set of Kubernetes objects for a two-variant
deployment, explains how the plugin discovers variants and their capacity, and
traces the plugin's behavior through each phase.

### Label-based discovery

The plugin has no static configuration. It discovers everything from label and
annotation matching at runtime each tick.

**Step 1 — find managed HPAs and group by model**

The plugin lists all `client.Object` items passed to `Tick` by the coordinator.
Those are HPAs and ScaledObjects carrying `llm-d.ai/managed: "true"`. It groups
them by `(namespace, llm-d.ai/model-id)`:

```
HPA: granite-3b-h100   annotations: model-id=ibm/granite-3b  accelerator-preference=1
HPA: granite-3b-a100   annotations: model-id=ibm/granite-3b  accelerator-preference=2
                                     ───────────────────────
                                     same model-id → same group
```

One group is formed: `(inference, ibm/granite-3b)`. Within that group, variants
are sorted by rank — H100 at rank 1, A100 at rank 2.

**Step 2 — map group to InferencePool**

In llm-d, one InferencePool is created per model. Because this design assumes
`1 model-id = 1 InferencePool`, grouping by `model-id` is equivalent to grouping
by InferencePool — no additional annotation is needed. Both HPAs serve the same
InferencePool (`ibm/granite-3b`) and the plugin treats them as a unit.

**Step 3 — discover capacity quota for the preferred variant**

To decide whether to park or unlock the A100, the plugin needs to know how much
spare capacity the H100 variant has. It looks up a ResourceQuota in the same
namespace whose labels exactly match the H100 HPA's annotations:

```
HPA annotation                        →   ResourceQuota label
──────────────────────────────────────    ──────────────────────────────────────
llm-d.ai/model-id: ibm/granite-3b    →   llm-d.ai/model-id: ibm/granite-3b
llm-d.ai/accelerator-preference: 1   →   llm-d.ai/accelerator-preference: 1
```

The plugin issues:
```
kubectl get resourcequotas -n inference \
  -l llm-d.ai/model-id=ibm/granite-3b,llm-d.ai/accelerator-preference=1
```

Exactly one quota must match. If zero or more than one match, the plugin skips
the group for that tick and logs a `Warning`.

The full label ownership picture for this scenario:

```
InferencePool (ibm/granite-3b)
│
├── HPA: granite-3b-h100  ──── annotations ────►  model-id=ibm/granite-3b
│                                                   accelerator-preference=1
│                                                          │
│                                        matched by labels ▼
│                               ResourceQuota: granite-3b-h100-quota
│                                   labels: model-id=ibm/granite-3b
│                                           accelerator-preference=1
│                                   hard: 8 GPUs
│
└── HPA: granite-3b-a100  ──── annotations ────►  model-id=ibm/granite-3b
                                                    accelerator-preference=2
                                                    preference-manifest-max=4  (written by plugin)
```

The A100 HPA has no quota — in a two-variant setup nothing reads it as a gate.
Only the H100 quota is needed.

---

### Phase 1 — Steady state (H100 quota has spare)

The operator has provisioned a quota capping H100 GPU usage at 8, and two HPAs —
H100 as the preferred variant and A100 as the fallback. The A100 `minReplicas` is
`0` so it can scale to zero completely when parked.

```yaml
# ResourceQuota for the H100 variant.
# Labels match the H100 HPA annotations so the plugin can discover this quota
# without any pointer annotation on the HPA itself.
apiVersion: v1
kind: ResourceQuota
metadata:
  name: granite-3b-h100-quota
  namespace: inference
  labels:
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "1"
spec:
  hard:
    requests.nvidia.com/gpu: "8"
---
# H100 HPA — rank 1 (most preferred). The plugin never modifies this HPA.
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-3b-h100
  namespace: inference
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "1"
spec:
  minReplicas: 2
  maxReplicas: 8
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: granite-3b-h100
  metrics: [...]
---
# A100 HPA — rank 2 (fallback). The plugin parks or unlocks this HPA.
# minReplicas: 0 means the deployment scales to zero when parked.
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: granite-3b-a100
  namespace: inference
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "2"
    # llm-d.ai/preference-manifest-max is absent at creation;
    # the plugin writes it on first observation (see Phase 2 below).
spec:
  minReplicas: 0
  maxReplicas: 4
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: granite-3b-a100
  metrics: [...]
```

On the first coordinator tick the plugin:
1. Groups both HPAs under `(inference, ibm/granite-3b)`.
2. Reads the H100 quota: `hard=8, used=4` (2 pods × 2 GPUs each) → `spare=4`.
3. Writes `llm-d.ai/preference-manifest-max: "4"` to the A100 HPA (first observation).
4. Parks the A100 HPA: patches `spec.maxReplicas = 0` (= `minReplicas`).

After tick 1 the A100 HPA looks like this in the cluster:

```yaml
metadata:
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "2"
    llm-d.ai/preference-manifest-max: "4"   # written by plugin
spec:
  minReplicas: 0
  maxReplicas: 0   # patched by plugin — no headroom, deployment at zero
```

### Phase 2 — H100 quota exhausted (fallback unlocked)

Load increases. The H100 HPA scales from 2 → 4 replicas, consuming all 8 GPUs:

```
granite-3b-h100-quota  hard=8  used=8  spare=0
```

On the next tick the plugin reads `spare=0` and unlocks the A100 HPA by restoring
`spec.maxReplicas` to `manifestMax`:

```yaml
metadata:
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-3b"
    llm-d.ai/accelerator-preference: "2"
    llm-d.ai/preference-manifest-max: "4"   # unchanged
spec:
  minReplicas: 0
  maxReplicas: 4   # restored by plugin — HPA now has headroom
```

The A100 HPA controller sees headroom and scales from 0 → 2 replicas on its demand
signal (same EPP queue depth metric as the H100 HPA).

### Phase 3 — Load subsides (fallback parked again)

Traffic drops. The H100 HPA scales back to 2 replicas, freeing 4 GPUs:

```
granite-3b-h100-quota  hard=8  used=4  spare=4
```

On the next tick the plugin reads `spare=4 > 0` and parks the A100 HPA:

```yaml
spec:
  minReplicas: 0
  maxReplicas: 0   # parked again — A100 Deployment scales to zero
```

### State summary

| H100 quota spare | Plugin action | A100 `spec.maxReplicas` | A100 replicas |
|---|---|---|---|
| > 0 | park | 0 (= `minReplicas`) | 0 |
| 0 | unlock | 4 (= `manifestMax`) | 0–4 |
| > 0 (recovered) | park | 0 (= `minReplicas`) | 0 |

---

## Implementation

The full implementation plan is at `docs/plans/replica-preference.md`. Summary:

| Phase | What changes |
|---|---|
| 1 — Annotation schema | Add `AcceleratorPreference` and `PreferenceManifestMax` constants to `internal/annotations/annotations.go` |
| 2 — Plugin | New package `internal/coordinator/plugins/preference/` with `plugin.go` and `algorithm.go` |
| 3 — Feature flag | Add `PREFERENCE_REBALANCE_ENABLED` (default `false`) to `internal/config/` |
| 4 — Wiring | Conditionally register the plugin in the `cfg.CoordinatorEnabled()` block in `cmd/main.go` |
| 5 — Observability | Three Prometheus metrics (`wva_preference_parked_total`, `wva_preference_unlocked_total`, `wva_preference_spare_gpus`) and structured `Info` logs on every park/unlock |

No new CRDs, no new controllers, no Prometheus dependency in the plugin.

---

## Rollout

1. Add `llm-d.ai/accelerator-preference` to all HPAs for this model. Confirm each
   HPA already carries `llm-d.ai/model-id` — it is required for grouping.
2. Create a `ResourceQuota` for every variant that gates another — every variant
   except the lowest-rank one. Each quota must carry `llm-d.ai/model-id` and
   `llm-d.ai/accelerator-preference` labels matching the corresponding HPA's
   annotations. For a two-variant setup (H100 rank 1, A100 rank 2) only the H100
   quota is required. For three variants, H100 and A100 quotas are both required.
3. Verify exactly one ResourceQuota exists per variant per model — duplicate labels
   cause the plugin to skip that model group with a warning:
   ```
   kubectl get resourcequotas -n <ns> -l llm-d.ai/model-id=<model-id>
   ```
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
- `algorithm_test.go`: single variant (no-op), equal ranks (no-op), preferred spare > 0
  (fallback parked), preferred spare = 0 (fallback unlocked), floor guard, ceiling
  guard.
- `spareGPUs` tests: quota found with spare, quota fully consumed, GPU key absent
  in quota (spare = 0), no matching quota (→ error).

### Integration Tests (`make test`)

- Preferred quota has spare → assert fallback `maxReplicas` set to `minReplicas`.
- Preferred quota exhausted → assert fallback `maxReplicas` restored to `manifestMax`.
- No ResourceQuota matching preferred variant labels → assert no patch, warning logged.
- Manifest persistence: park fallback, restart plugin, assert unlock restores the
  correct `manifestMax` from annotation.
- KEDA path: fallback is a ScaledObject → assert `spec.maxReplicaCount` is patched.

### E2E Tests (`make test-e2e-smoke`)

1. Light load: H100 quota has spare → A100-sim stays at `minReplicas`.
2. Ramp load until H100 quota is exhausted → A100-sim is unlocked and scales up.
3. Load subsides → H100 quota recovers → A100-sim is parked again.

All images must use fully-qualified registry paths (`registry.k8s.io/`, `quay.io/`,
or a private registry). Docker Hub is not permitted.

