# Dump EPP Flow Control Metrics

This script (`dump_epp_fc_metrics.py`) automates extracting Prometheus metrics data related to the Gateway API Inference Extension's Endpoint Picker (EPP) and Flow Control mechanisms.

Because the tool depends on GuideLLM benchmark timestamps to specify the time window, **it is designed to run after a GuideLLM benchmark is completed**.

## Overview

After a complete benchmark run, this script parses the output YAML files in the results directory to identify the exact start and end times of the test. It pads this window by 60 seconds on both sides, then queries OpenShift's user workload monitoring stack for Flow Control, pool-level, and request-level metrics. 

It saves the raw time-series metrics data as a JSON dump directly into the specified results directory.

## Prerequisites

- **Python 3+**
- `pyyaml`
- `get_benchmark_report.py` must be present in the same directory or accessible in the Python path, as this script uses its authentication and Prometheus querying functionalities.
- An active OpenShift `oc` session with `cluster-admin` (or equivalent privileges to query user workload monitoring), OR explicit OpenShift token and server URL arguments must be provided.

## Usage

```bash
./dump_epp_fc_metrics.py --results-dir <PATH_TO_GUIDELLM_RESULTS> [OPTIONS]
```

### Arguments

| Argument | Short | Description | Default |
| :--- | :---: | :--- | :--- |
| `--results-dir` | `-r` | **(Required)** Path to the GuideLLM results directory. Used to read the YAML summary boundaries and output the metric dump file. | |
| `--namespace` | `-n` | The target namespace. | `asmalvan-test` |
| `--token` | `-t` | OpenShift login token. | `None` |
| `--server` | `-s` | OpenShift API server URL. | `None` |

## Output

The script generates a single JSON file named **`epp_metrics_dump.json`** inside the target directory specified by `--results-dir`.

This JSON file contains Prometheus time-series data scraped at 15s intervals for various metrics, including:
- **Flow Control Metrics**: Request queue sizes (`inference_extension_flow_control_queue_size`), enqueue and dispatch durations, and pool saturation.
- **EPP Objective Metrics**: Total requests, running requests, request duration, total input/output tokens, and normalized TTFT.
- **EPP Pool Metrics**: Average per-pod queue depth, ready pods count, and average KV cache utilization.
