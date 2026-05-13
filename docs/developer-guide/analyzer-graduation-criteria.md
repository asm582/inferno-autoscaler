# Analyzer Graduation Criteria

This document defines the benchmark-based graduation criteria for introducing a new scaling analyzer into WVA. All criteria are evaluated against the **current default analyzer's** results recorded in [`docs/benchmark.md`](../benchmark.md).

## Reference Workloads

Every candidate analyze in WVA must be benchmarked against all three canonical workload profiles or create new profiles relevant to analyzer's behavior. Benchmarking should be done using llmd components by installing Gateway and the scheduler on GPU cluster.

| Scenario | Input Tokens | Output Tokens | Request Rate | Duration |
|---|---|---|---|---|
| **Prefill-heavy** | 4000 | 1000 | 20 RPS (Poisson) | 600s |
| **Decode-heavy** | 1000 | 4000 | 20 RPS (Poisson) | 600s |
| **Symmetrical** | 1000 | 1000 | 20 RPS (Poisson) | 600s |

## Reference Environment

Benchmark results are only comparable when run under identical conditions:

- **Hardware**: NVIDIA H100 (OpenShift cluster)
- **Model**: The model specified in `docs/benchmark.md` (currently Qwen/Qwen3-32B)
- **Load generator**: GuideLLM or inference-perf with Poisson arrival profile
- **HPA settings**: As documented in the HPA Configuration section of `docs/benchmark.md`

Results collected under different hardware, models, or load patterns are informational but may not count toward graduation.

## Measured Metrics

The following metrics are collected for each benchmark run:

| Metric | What It Measures |
|---|---|
| **P99 TTFT** (ms) | Worst-case time to first token — user-perceived responsiveness |
| **P99 ITL** (ms/token) | Worst-case inter-token latency — streaming quality |
| **Error rate** | Fraction of requests that failed (timeouts, 5xx, etc.) |
| **Avg replicas** | Mean replica count over the run — proxy for cost |
| **Max replicas** | Peak replica count reached — headroom usage |
| **Avg KV cache utilization** | Mean KV cache pressure — how close to memory saturation |
| **Avg queue depth** | Mean EPP queue depth — scaling responsiveness indicator |

## Graduation Bar

A new analyzer **graduates** (is accepted into the codebase as an production option) when it meets **all** of the following conditions across **all three** workload profiles:

### 1. No latency regression

P99 TTFT and P99 ITL must be **< the current default analyzer's baseline** in relevant scenarios. An analyzer that improves one scenario but regresses another may not pass.

### 2. No error rate regression

The error rate (failed requests / total requests) must be **≤ the current default's baseline** in relevant scenario(s).

### 3. Cost efficiency within bounds

Average replica count must be **within ±10%** of the current default at equivalent or better latency. An analyzer that achieves marginal latency gains by scaling to maximum replicas fails this criterion.

### 4. Scaling responsiveness preserved

Average queue depth must **not exceed the current default by more than 20%**. Higher queue depth indicates the analyzer is reacting too slowly to incoming load, causing upstream request queuing.

### 5. All scenarios required

Results must be reported for prefill-heavy, decode-heavy, **and** symmetrical workloads. Cherry-picking a favorable scenario may not be permitted.

## Promotion to Default

Graduation allows an analyzer to exist as a selectable option. To **replace the current default**, the bar is higher. The candidate must demonstrate improvement on **two** of the following without regressing any:

| Improvement Target | Threshold |
|---|---|
| P99 TTFT reduction | > 20% lower than current default |
| Cost reduction | > 10% fewer avg replicas at equal or better latency |

## Recording Results

All benchmark results must be added to [`docs/benchmark.md`](../benchmark.md) in a new section following the existing format:

```markdown
<Scenario Name>

**llm-d Release:** <version>
**Model:** <model name>
**Workload:** <input tokens>, <output tokens>, <RPS>, <duration>
**Saturation Engine:** <analyzer name and version>

| Metric | <Analyzer A> | <Analyzer B> |
|--------|-------------|-------------|
| P99 TTFT (ms) | ... | ... |
| P99 ITL (ms/token) | ... | ... |
| Avg replicas | ... | ... |
| Max replicas | ... | ... |
| Avg KV cache utilization | ... | ... |
| Avg queue depth (EPP) | ... | ... |
| Error count | ... | ... |
| Cost (avg replicas × GPU/hr) | ... | ... |
```

Include the analyzer's configuration parameters (thresholds, tuning knobs) alongside the results so that others can reproduce the run.
