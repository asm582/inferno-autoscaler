# Benchmark Report Generator

This script (`get_benchmark_report.py`) interacts with an OpenShift cluster to gather metrics and build a comprehensive benchmark report PDF. It captures data from Prometheus (both cluster and user-workload metrics) and parses GuideLLM results to generate text analysis and plots (e.g., KV cache usage, queued requests, replicas, TTFT, and ITL).

## Prerequisites & Installation

To run this script locally, ensure you have python installed and the required dependencies:

```bash
pip install matplotlib pyyaml
```

The script also relies on having the OpenShift CLI tool (`oc`) installed and accessible in your system's PATH.

## OpenShift Privileges (`oc`)

You must be logged into an OpenShift cluster with sufficient privileges. The script attempts to verify these permissions before executing.

- **Required Privileges**: You need `cluster-admin` rights, or a custom role that grants the ability to `create pods/exec` in both the `openshift-monitoring` and `openshift-user-workload-monitoring` namespaces. Wait time checks, fetching pod states, and querying Prometheus directly all rely on executing `curl` from inside prometheus pods.
- If you are running this in a CI/CD environment, you can pass `-t`/`--token` and `-s`/`--server` arguments to have the script log in automatically via `oc login`.

## Usage & Arguments

```bash
python get_benchmark_report.py [OPTIONS]
```

| Argument | Short | Default | Description |
|---|---|---|---|
| `--namespace` | `-n` | `asmalvan-test` | The Kubernetes namespace containing the workload to query metrics for. |
| `--window` | `-w` | `1h` | The time window for Prometheus queries (e.g., `30m`, `1h`, `2h`). |
| `--output` | `-o` | `metrics_usage.png` | Output base name for the plot files. Generates both `.png` and `.pdf` files. |
| `--results-dir` | `-r` | `None` | Path to a GuideLLM `exp-docs` results directory. |
| `--token` | `-t` | `None` | OpenShift login token. |
| `--server` | `-s` | `None` | OpenShift API server URL. |

## Path to GuideLLM Results

To include GuideLLM latency calculations, true serving capacity estimates, and performance scoring in your output, you **must** supply the path to the GuideLLM results directory.

Example:
```bash
python get_benchmark_report.py -n my-namespace -r /path/to/exp-docs
```

When you use the `-r` (or `--results-dir`) flag:
- The script looks for `results.json` to calculate Successful vs Failed RPS, P99 TTFT, and P99 ITL.
- It will also read the `*_results.json_*.yaml` parameter files in that directory to tightly clamp the Prometheus time windows precisely onto the start/stop time of the benchmark phases instead of relying strictly on the `--window` parameter.
- The output files will be automatically named based on the basename of the results directory (e.g., `metrics_usage_exp-docs.pdf`).
