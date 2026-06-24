# Replica Preference

## Context

WVA currently has no mechanism to express "scale H100s first; use A100s only when
H100 capacity is full." The proposal at `docs/proposals/replica-preference.md`
adds a new coordinator plugin that gates fallback-variant HPAs using Kubernetes
`ResourceQuota` as the capacity signal. The preferred variant's HPA is never
touched. Variants are grouped by `(namespace, llm-d.ai/model-id)` — the same
grouping key already used by the rest of WVA — so preference is expressed per
model within a namespace.

The internal saturation pipeline, analyzers, and cost-aware optimization do **not**
change. Only the coordinator gains a new plugin.

This plan implements the proposal across four phases:

- **Phase 1** — Annotation schema: add two new constants to the annotations package.
- **Phase 2** — Plugin: implement the `preference` coordinator plugin.
- **Phase 3** — Feature flag: wire the flag into the config package.
- **Phase 4** — Coordinator wiring: register the plugin in `cmd/main.go`.
- **Phase 5** — Observability: Prometheus metrics and structured logging.

---

## Critical files

**Annotations**
- `internal/annotations/annotations.go` — add `AcceleratorPreference` and `PreferenceManifestMax` constants

**New plugin package**
- `internal/coordinator/plugins/preference/plugin.go` — `Plugin` struct, `Name()`, `Tick()`
- `internal/coordinator/plugins/preference/algorithm.go` — `resolveTiers()`, `spareGPUs()`, `manifestMax()`

**Config**
- `internal/config/loader.go` — `v.SetDefault("PREFERENCE_REBALANCE_ENABLED", false)`
- `internal/config/config.go` — `PreferenceEnabled() bool` method

**Wiring**
- `cmd/main.go` — conditionally append `preference.New(mgr.GetClient())` inside the `cfg.CoordinatorEnabled()` block

**Tests**
- `internal/annotations/annotations_test.go` — extend with preference annotation cases
- `internal/coordinator/plugins/preference/algorithm_test.go` — unit tests for algorithm
- `test/e2e/` — smoke scenario covering park → unlock → park

---

## Phase 1 — Annotation Schema

Add constants to `internal/annotations/annotations.go` following the existing
`const` block pattern:

```go
// AcceleratorPreference ranks variants of the same model by preference.
// Lower value = higher preference. Defaults to 100 when absent.
AcceleratorPreference = "llm-d.ai/accelerator-preference"

// PreferenceManifestMax stores the operator-intended maxReplicas for fallback
// variants. Written by the plugin on first observation. Do not set manually.
PreferenceManifestMax = "llm-d.ai/preference-manifest-max"

const defaultAcceleratorPreference = 100
```

### 1.1 Tests

Extend `internal/annotations/annotations_test.go`:

- `AcceleratorPreference` absent → defaults to 100
- `AcceleratorPreference` zero or negative → error
- `AcceleratorPreference` valid integer → parsed correctly

---



## Phase 2 — Coordinator Plugin

Create the package `internal/coordinator/plugins/preference/` with two files.

### 2.1 `plugin.go`

Plugin struct and coordinator interface implementation:

```go
type Plugin struct {
    client client.Client
}

func New(c client.Client) *Plugin { return &Plugin{client: c} }
func (p *Plugin) Name() string    { return "preference" }
```

`Tick` groups the incoming `[]client.Object` by `(namespace, model-id)` inline
(no helper needed — it is a map-build loop), then calls `resolveTiers` for each
group. HPAs missing the `llm-d.ai/model-id` annotation are skipped.

RBAC markers live in this file:

```go
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;patch;update
```

Regenerate `config/rbac/role.yaml` via `make manifests` after adding these markers.

### 2.2 `algorithm.go`

Three functions:

**`resolveTiers(ctx, ns, modelID, variants)`**

Sorts variants by rank ascending. Skips groups with fewer than 2 distinct ranks.
For each variant with rank > 1, reads `spareGPUs` from the variant immediately
above (rank - 1). Calls `setMaxReplicas` to park or unlock based on the result:

```
spare > 0  → park:   setMaxReplicas(variant, variant.minReplicas)
spare == 0 → unlock: setMaxReplicas(variant, manifestMax(variant))
```

Skips the patch when `newMax == currentMax`. Never sets `maxReplicas > manifestMax`.

**`spareGPUs(ctx, ns, modelID, rank) (int64, error)`**

Lists `ResourceQuota` objects in the namespace filtered by labels
`llm-d.ai/model-id=<modelID>` and `llm-d.ai/accelerator-preference=<rank>`.
Returns `hard - used` for the GPU resource key (inferred via
`internal/utils/utils.go` vendor detection). Error cases:

- No matching quota → return error (caller skips variant, logs Warning)
- Multiple matching quotas → return error (caller skips group, logs Warning)
- GPU key absent in quota → `hard=0, used=0, spare=0` → fallback unlocked
- `hard=0` → log Warning, return 0 (fallback unlocked)

**`manifestMax(tier) (int32, error)`**

Reads `llm-d.ai/preference-manifest-max` from the tier's annotations. If absent,
writes `currentMax` to the annotation (first observation) and returns it. If the
operator has since raised `maxReplicas` above the stored value, adopts the new
value. Returns error only on patch failure.

For KEDA ScaledObjects, `setMaxReplicas` patches `spec.maxReplicaCount` instead
of `spec.maxReplicas` — patching the KEDA-generated HPA would be reverted on
KEDA's next reconcile.

### 2.3 Tests

`internal/coordinator/plugins/preference/algorithm_test.go`:

- Single variant → no-op
- Two variants at the same rank → no-op
- Preferred spare > 0 → fallback `maxReplicas` set to `minReplicas`
- Preferred spare == 0 → fallback `maxReplicas` set to `manifestMax`
- Ceiling guard: `newMax` never exceeds `manifestMax`
- `spareGPUs`: quota found with spare, quota fully consumed, GPU key absent (spare=0),
  no matching quota (error), multiple matching quotas (error)
- `manifestMax`: absent on first tick (written), operator raises value (adopted)
- KEDA path: `spec.maxReplicaCount` patched on ScaledObject

---

## Phase 3 — Feature Flag

Add the flag following the existing viper pattern used by `EXPERIMENTAL_COORDINATOR_ENABLED`.

**`internal/config/loader.go`**

```go
v.SetDefault("PREFERENCE_REBALANCE_ENABLED", false)
```

**`internal/config/config.go`**

```go
// PreferenceEnabled reports whether the preference coordinator plugin is enabled.
func (c *Config) PreferenceEnabled() bool {
    return c.v.GetBool("PREFERENCE_REBALANCE_ENABLED")
}
```

Default is `false`. When `false`, `Tick` returns immediately without reading or
writing anything.

---

## Phase 4 — Coordinator Wiring

**`cmd/main.go`** — inside the existing `cfg.CoordinatorEnabled()` block:

```go
plugins := []coordinator.Plugin{
    gpurebalance.New(mgr.GetClient(), promAPI),
}
if cfg.PreferenceEnabled() {
    plugins = append(plugins, preference.New(mgr.GetClient()))
}
```

Add the import for `preference` alongside the existing `gpurebalance` import.

---

## Phase 5 — Observability

### 5.1 Prometheus metrics

Register the following metrics in `plugin.go` using `promauto` (matching the
pattern used elsewhere in the coordinator):

| Metric | Type | Labels |
|---|---|---|
| `wva_preference_parked_total` | Counter | `namespace`, `model_id`, `hpa` |
| `wva_preference_unlocked_total` | Counter | `namespace`, `model_id`, `hpa` |
| `wva_preference_spare_gpus` | Gauge | `namespace`, `model_id`, `rank` |

### 5.2 Structured logging

Every park and unlock emits at `Info` level from `resolveTiers`:

```
action=park|unlock  hpa=<name>  namespace=<ns>  model_id=<model-id>
from=<old>  to=<new>  spare_gpus=<n>  rank=<n>
```

Quota read errors emit at `Warning` level with `namespace`, `model_id`, and `rank` fields.

---

## Verification

**Per phase**

- Phase 1: `make test` green; annotation parsing tests pass.
- Phase 2: `make test` green; all algorithm unit tests pass; RBAC markers regenerated
  (`make manifests` produces no diff after adding markers).
- Phase 3: Config unit tests confirm `PreferenceEnabled()` returns `false` by default
  and `true` when set.
- Phase 4: `make build` succeeds; with `PREFERENCE_REBALANCE_ENABLED=false` the plugin
  is registered but inactive; with `true` it runs.
- Phase 5: `kubectl get --raw /metrics | grep wva_preference` shows all three series.

**End-to-end smoke (`make test-e2e-smoke`)**

1. Light load: H100 quota has spare → A100-sim `maxReplicas` equals `minReplicas` (parked).
2. Ramp load until H100 quota exhausted → A100-sim `maxReplicas` restored to `manifestMax`.
3. Load subsides, H100 quota recovers → A100-sim parked again.

All images must use fully-qualified registry paths (`registry.k8s.io/`, `quay.io/`,
or a private registry). Docker Hub is not permitted.

**Regression surface to watch**

- `gpu-rebalance` plugin behavior is unchanged — preference plugin operates on
  non-overlapping annotation keys and only touches fallback HPAs.
- `manifestMax` annotation must survive manager restarts — verify by restarting the
  manager between park and unlock in the e2e scenario.
- KEDA path: confirm `spec.maxReplicaCount` on the ScaledObject is patched, not
  the KEDA-generated HPA.
