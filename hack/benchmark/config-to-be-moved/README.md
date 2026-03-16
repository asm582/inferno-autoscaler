# Benchmarking Configurations

This directory (`config-to-be-moved`) contains benchmarking configurations and scenario scripts that are currently maintained here but **need to be moved or upstreamed to the `llm-d-benchmark` repository** in the future.

## Files and Destinations

To run the benchmarking experiments properly, copy these files into your cloned `llm-d-benchmark` repository:

### 1. Scenario Script (`inference-scheduling.sh`)

This script includes the configuration required to enable the **flow controller** feature gate. 

**Destination:**
Copy `inference-scheduling.sh` to the `scenarios/guides/` directory in the `llm-d-benchmark` repository.

```bash
cp inference-scheduling.sh /path/to/llm-d-benchmark/scenarios/guides/inference-scheduling.sh
```

### 2. Workload Profile (`chatbot_synthetic.yaml.in`)

This GuideLLM profile file includes a `seed` parameter required for reproducible synthetic load generation.

**Destination:**
Copy `chatbot_synthetic.yaml.in` to the `workload/profiles/guidellm/` directory in the `llm-d-benchmark` repository.

```bash
cp chatbot_synthetic.yaml.in /path/to/llm-d-benchmark/workload/profiles/guidellm/chatbot_synthetic.yaml.in
```

*(Note: If you are using a specific environment like `llmd-bench-pokprod`, your destination path might look like `/Users/abhishekmalvankar/llmd-bench-pokprod/llm-d-benchmark/workload/profiles/guidellm/chatbot_synthetic.yaml.in`)*
