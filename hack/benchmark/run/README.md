# CI Benchmark Runner

This directory contains the `run_ci_benchmark.sh` script, which is used to automate the end-to-end benchmarking of the Workload Variant Autoscaler (WVA) in a CI/CD environment.

## Setup Instructions

This script is designed to be executed from within the `llm-d` benchmark repository. To use it, you must first clone the repository and then place this script in a specific directory structure.

### 1. Clone the LLM-D Repository

First, clone the `llm-d-benchmark` repository (or your fork of it) to your local machine or CI runner:

```bash
git clone https://github.com/kubernetes-sigs/llm-d-benchmark.git
cd llm-d-benchmark
```

### 2. Place the Script

The `run_ci_benchmark.sh` script relies on relative paths to find the setup scripts and scenarios within the `llm-d` repository. Specifically, it resolves the repository root as the parent directory of the script's location:
```bash
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
```

Therefore, you **must** create a directory exactly one level deep inside the cloned `llm-d-benchmark` repository and place the script there. 

For example, create a `scripts` directory in the root of the cloned repo:

```bash
mkdir -p scripts
cp /path/to/inferno-autoscaler/hack/benchmark/run/run_ci_benchmark.sh scripts/
```

This ensures that the script correctly locates `setup/env.sh`, `setup/teardown.sh`, `setup/standup.sh`, and the scenario files.

### 3. Run the Benchmark

Once the script is in the correct location (e.g., `llm-d-benchmark/scripts/run_ci_benchmark.sh`), you can execute it:

```bash
cd scripts
./run_ci_benchmark.sh -n "my-namespace" -m "Qwen/Qwen3-0.6B" -s "inference-scheduling"
```

## Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `-n` | `asmalvan-test` | The Kubernetes namespace to use for the benchmark. |
| `-m` | `Qwen/Qwen3-0.6B` | The model to deploy and benchmark. |
| `-s` | `inference-scheduling` | The scenario file to use during the standup phase. |
| `-w` | `chatbot_synthetic` | The workload profile to simulate (e.g., `chatbot_synthetic`). |

## Direct HPA Experiment (Bypassing WVA)

In some cases, you may want to run a baseline benchmark using the standard Kubernetes Horizontal Pod Autoscaler (HPA) directly attached to the model server metrics, bypassing the Workload Variant Autoscaler entirely.

To do this:
1. Wait for the standard WVA stack to come up as part of the normal standup process (e.g., by running `run_ci_benchmark.sh` and stopping or pausing before the benchmark execution). This step involves deploying the WVA stack with two decode replicas initially. Let WVA successfully scale this down to one replica. This verifies that the external metrics machinery is fully operational before proceeding.
2. Once the environment is up, scale down the WVA controller and patch the HPA object by applying the provided manifest:

```bash
oc apply -f scripts/bypass_wva_direct_hpa.yaml
```

This manifest will:
- Scale the `workload-variant-autoscaler-controller-manager` deployment down to 0 replicas, effectively disabling WVA.
- Deploy a Direct HPA (`workload-variant-autoscaler-hpa`) targeting the `qwen-qwe-...-decode` deployment (your model server).
- Configure the HPA to scale based on `inference_extension_flow_control_queue_size` and `inference_objective_running_requests` using the Prometheus Adapter.

After applying the configuration, you can execute the load generator to test the native HPA scaling behavior.
