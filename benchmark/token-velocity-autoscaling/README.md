# Token-Velocity-Aware Autoscaling Benchmark

End-to-end toolkit for benchmarking **token-velocity-aware autoscaling** on an OpenShift
cluster with GPUs. It deploys the upstream llm-d P/D disaggregation guide, calibrates
`peakPrefillThroughput` (V_P) on the live hardware, creates KEDA ScaledObjects with
token-aware triggers, validates the metric pipeline, and runs staged-ramp workloads that
exercise autoscaling under controlled load.

See [TOKEN-AWARE-AUTOSCALING-SUMMARY.md](TOKEN-AWARE-AUTOSCALING-SUMMARY.md) for the
theory, threshold derivation math, PromQL query design, and calibration methodology.

## Prerequisites

| requirement | notes |
|---|---|
| **OpenShift 4.14+** | k8s >= 1.29 (native sidecar support for the decode routing proxy) |
| **GPU nodes** | minimum 4 GPUs for the default topology (prefill 1xTP2 + decode 1xTP2) |
| **User Workload Monitoring** | enabled cluster-wide (`openshift.io/user-monitoring`) |
| **KEDA 2.12+** | with Prometheus trigger support |
| **GAIE CRDs** | `InferencePool` v1 (`inference.networking.k8s.io`) |
| **CLI tools** | `kubectl`/`oc`, `helm`, `kustomize`, `python3` (with `pyyaml`), `git` |
| **llmdbenchmark CLI** | from [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark) |

### Install llmdbenchmark

```bash
git clone https://github.com/llm-d/llm-d-benchmark.git
cd llm-d-benchmark
./install.sh
source .venv/bin/activate
```

## Quick Start

The five scripts run in order. Each assumes the previous step succeeded.

```bash
cd benchmark/token-velocity-autoscaling

# 1. Deploy the P/D stack with monitoring
./deploy-pd-guide.sh --model-cache

# 2. Calibrate peakPrefillThroughput on this hardware and patch the EPP
./calibrate-peak-prefill.sh --apply
#    Note the V_P value printed (e.g. 2787 tok/s)

# 3. Create KEDA ScaledObjects with token-aware triggers
#    Derive the prefill threshold: threshold = TTFT_SLO - (ISL / V_P)
#    Example: 4.0 - (8192 / 2787) = 1.061
./launch-scaledobjects.sh --vp 2787 --prefill-threshold 1.061

# 4. Validate the metric pipeline (must exit 0 before proceeding)
./test-metric-flow.sh

# 5. Run the benchmark
LLMDBENCH_BASE_DIR=/path/to/llm-d-benchmark \
  ./run-benchmark.sh \
    --workload-file workloads/pd-autoscaling-ramp-prefill-heavy.yaml \
    --model "Qwen/Qwen3-32B"
```

Results land in `./benchmark-results/<timestamp>/` with an `autoscaling-timeline.csv` recording
replica counts and trigger values throughout the run.

## Scripts

### deploy-pd-guide.sh

Deploys the upstream llm-d `pd-disaggregation` guide end-to-end: clone/checkout, clean namespace,
HF token, optional model-weight PVC, helm install router, kustomize overlay for model servers,
monitoring (PodMonitors + EPP ServiceMonitor with bearer auth), wait for rollout, verification.

```bash
./deploy-pd-guide.sh                   # full cycle: clean ns -> deploy -> verify
./deploy-pd-guide.sh --model-cache     # share one weight download across pods
./deploy-pd-guide.sh --dry-run         # preflight + render + assert, apply nothing
./deploy-pd-guide.sh --verify-only     # re-run verification against a live stack
./deploy-pd-guide.sh --teardown        # helm uninstall + delete namespace
```

Key environment overrides: `NAMESPACE` (default `pd-test`), `MODEL` (default `Qwen/Qwen3-32B`),
`PREFILL_REPLICAS`, `PREFILL_TP`, `DECODE_REPLICAS`, `DECODE_TP`.

### calibrate-peak-prefill.sh

Measures `peakPrefillThroughput` (V_P) using the upstream calibration Job. Runs N repeats (default
3), reports median and spread, and optionally patches the EPP ConfigMap via `helm upgrade`.

```bash
./calibrate-peak-prefill.sh            # measure only
./calibrate-peak-prefill.sh --apply    # measure and patch the EPP
./calibrate-peak-prefill.sh --dry-run  # show what would happen
```

### launch-scaledobjects.sh

Creates the KEDA auth chain (ServiceAccount, token Secret, ClusterRoleBinding,
TriggerAuthentication) and two ScaledObjects:
- **Prefill** (`prefill-tokenaware`): `inflight_tokens / V_P` — seconds of backlog
- **Decode** (`decode-tokenaware`): `kv_cache_usage_perc` — KV memory occupancy

```bash
./launch-scaledobjects.sh --vp 2787 --prefill-threshold 1.061
./launch-scaledobjects.sh --vp 2787 --prefill-threshold 1.061 --max 4   # cap at 4 replicas
./launch-scaledobjects.sh --delete                                       # remove ScaledObjects
```

### test-metric-flow.sh

Validates the full metric pipeline: EPP `/metrics` -> Prometheus (user-workload) -> Thanos
Querier -> KEDA -> HPA. Queries use the metrics-reader SA's own token (the same credential KEDA
uses), not a cluster-admin token. **Must exit 0 before running the benchmark** — otherwise KEDA
may silently fall back to `or vector(0)` and autoscaling will never fire.

```bash
./test-metric-flow.sh                  # health check (read-only)
./test-metric-flow.sh --probe 180      # drive load and prove metrics move
./test-metric-flow.sh --watch 20       # sample an in-flight scaling event
```

### run-benchmark.sh

Runs `llmdbenchmark` against the deployed stack in run-only mode (never calls standup/teardown).
Records an autoscaling timeline CSV throughout the run.

```bash
./run-benchmark.sh -w workloads/pd-autoscaling-ramp-prefill-heavy.yaml --model Qwen/Qwen3-32B
./run-benchmark.sh --pause-autoscaling    # fixed-topology baseline
./run-benchmark.sh --dry-run
```

Requires `LLMDBENCH_BASE_DIR` pointing to a llm-d-benchmark checkout.

## Workload Profiles

| file | ISL | OSL | ratio | stages | duration | exercises |
|---|---|---|---|---|---|---|
| `pd-autoscaling-ramp.yaml` | 2048 | 2048 | 1:1 | 7 | 57 min | both triggers |
| `pd-autoscaling-ramp-prefill-heavy.yaml` | 8192 | 256 | 32:1 | 7 | 57 min | prefill trigger (`inflight_tokens / V_P`) |
| `pd-autoscaling-ramp-decode-heavy.yaml` | 256 | 8192 | 1:32 | 7 | 66 min | decode trigger (`kv_cache_usage_perc`) |

All use a 7-stage ramp (up -> peak -> down) with `streaming: true`, `num_workers: 64`,
`worker_max_concurrency: 100`.

## Threshold Derivation

The prefill trigger threshold is derived from the TTFT SLO:

```
threshold = TTFT_SLO - (ISL_uncached / V_P)
```

Where:
- `TTFT_SLO`: target time-to-first-token (e.g. 4.0 s)
- `ISL_uncached`: input sequence length for uncached requests (e.g. 8192)
- `V_P`: measured peakPrefillThroughput (e.g. 2787 tok/s)

Example: `4.0 - (8192 / 2787) = 1.061 s`

KEDA computes `replicas = ceil(metric_value / threshold)`. The metric value is the seconds of
prefill backlog (inflight tokens divided by V_P). When backlog exceeds the threshold, KEDA
scales up to absorb the excess.

The decode threshold (default 0.8) fires when aggregate KV-cache occupancy exceeds 80%.

## Expected Output

After `run-benchmark.sh` completes:

```
benchmark-results/<timestamp>/
├── autoscaling-timeline.csv              # replica counts + trigger values every 15s
├── <run-id>/results/<experiment>/
│   ├── stage_0_lifecycle_metrics.json    # per-stage latency/throughput
│   ├── stage_1_lifecycle_metrics.json
│   ├── ...
│   └── summary_lifecycle_metrics.json    # aggregate metrics
├── epp-metrics-before.txt
├── epp-metrics-after.txt
└── run-benchmark.log
```

The `autoscaling-timeline.csv` records `prefill_replicas`, `prefill_ready`, `decode_replicas`,
`decode_ready`, and the trigger metric values at each sample. Use it to correlate scaling events
with latency changes in the per-stage metrics.

## Troubleshooting

### `test-metric-flow.sh` stage 3 fails: "NO SERIES — metric absent, not zero"

**For `inflight_tokens`**: This metric is registered lazily on the first dispatched request. Send
one warmup request through the EPP and wait ~30s for Prometheus to scrape:

```bash
IP=$(kubectl get svc -n pd-test -l app.kubernetes.io/name=pd-disaggregation-epp \
       -o jsonpath='{.items[0].spec.clusterIP}')
kubectl run warmup --rm -i --restart=Never -n pd-test --image=cfmanteiga/alpine-bash-curl-jq \
  --command -- curl -s -m 120 -X POST "http://${IP}/v1/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen3-32B","prompt":"hello","max_tokens":8}'
```

**For `kv_cache_usage` or `per_endpoint_queue_size`**: Verify the PodMonitors and ServiceMonitor
exist and Prometheus targets are healthy:
```bash
kubectl get podmonitor,servicemonitor -n pd-test
```

### EPP ServiceMonitor returns HTTP 401

The EPP uses controller-runtime's secure metrics (SubjectAccessReview auth). The fix requires:
1. A ClusterRoleBinding granting the EPP SA `create tokenreviews`, `create subjectaccessreviews`,
   and `get /metrics` (nonResourceURL)
2. A token Secret for the EPP SA
3. The ServiceMonitor configured with `authorization.credentials` referencing that Secret

`deploy-pd-guide.sh` handles all three in its `enable_monitoring()` function.

### Cluster session expires during benchmark

The benchmark pod runs inside the cluster — it completes regardless of your local session. If
`run-benchmark.sh` exits with an auth error mid-run, re-login (`oc login`) and check the harness
pod status. Results can be recovered from the workload PVC.

### Scale-up pods are Pending

Each new replica needs TP GPUs on a single node. Check cluster GPU capacity:
```bash
kubectl get nodes -l nvidia.com/gpu.present=true \
  -o custom-columns='NODE:.metadata.name,ALLOC:.status.allocatable.nvidia\.com/gpu'
```
