#!/bin/bash
# ==============================================================================
# Automates the end-to-end benchmarking of the Workload Variant Autoscaler (WVA)
# Intended for execution natively within a CI/CD environment.
# ==============================================================================

# Fail immediately if a command fails, and echo all commands for CI logs
set -e
set -x

# --- Configuration Variables ---
NAMESPACE="asmalvan-test"
MODEL="Qwen/Qwen3-0.6B"
SCENARIO="inference-scheduling"
WORKLOAD_PROFILE="chatbot_synthetic"

while getopts ":n:m:s:w:" opt; do
  case $opt in
    n) NAMESPACE="$OPTARG" ;;
    m) MODEL="$OPTARG" ;;
    s) SCENARIO="$OPTARG" ;;
    w) WORKLOAD_PROFILE="$OPTARG" ;;
    \?) echo "Invalid option -$OPTARG" >&2; exit 1 ;;
  esac
done

# Absolute paths based on typical repository structure
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export LLMDBENCH_MAIN_DIR="${REPO_ROOT}"
export LLMDBENCH_CONTROL_CALLER="run_ci_benchmark.sh"
SCENARIO_PATH="${REPO_ROOT}/scenarios/guides/inference-scheduling.sh"

if [ -f "${REPO_ROOT}/setup/env.sh" ]; then
    echo "Sourcing environment variables from ${REPO_ROOT}/setup/env.sh..."
    source "${REPO_ROOT}/setup/env.sh"
else
    echo "⚠️ Warning: setup/env.sh not found. Ensure required variables (e.g., LLMDBENCH_HF_TOKEN) are exported."
fi

echo "============================================================================="
echo "▶️ STEP 1: Teardown Existing Environment"
echo "============================================================================="
"${REPO_ROOT}/setup/teardown.sh" -c "${SCENARIO_PATH}" -p "${NAMESPACE}" --deep

echo "Waiting for namespace ${NAMESPACE} to fully clear..."
# Polling loop to ensure all decode pods are gone
MAX_WAIT_CLEAN=60 # seconds
for (( i=1; i<=MAX_WAIT_CLEAN; i++ )); do
    POD_COUNT=$(oc get pods -n "${NAMESPACE}" -l "model=${MODEL##*/}" --no-headers 2>/dev/null | wc -l || true)
    if [ "${POD_COUNT}" -eq 0 ]; then
        echo "✅ Namespace cleanly purged."
        break
    fi
    echo "Wait ${i}/${MAX_WAIT_CLEAN}: Waiting for decode pods to terminate..."
    sleep 5
done


echo "============================================================================="
echo "▶️ STEP 2: Standup Stack with WVA"
echo "============================================================================="
"${REPO_ROOT}/setup/standup.sh" -p "${NAMESPACE}" -m "${MODEL}" -c "${SCENARIO}" --wva



echo "============================================================================="
echo "▶️ STEP 3: Verify HPA Scale-to-One/Zero Logic"
echo "============================================================================="
echo "Waiting for HPA to ingest metrics and scale decode pods down from default 2 to 1..."
HPA_NAME=$(oc get hpa -n "${NAMESPACE}" -o jsonpath="{.items[0].metadata.name}")

MAX_WAIT_SCALE=120
SCALED_DOWN=false
for (( i=1; i<=MAX_WAIT_SCALE; i++ )); do
    CURRENT_REPLICAS=$(oc get hpa "${HPA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.currentReplicas}')
    if [ "${CURRENT_REPLICAS}" -eq 1 ] || [ "${CURRENT_REPLICAS}" -eq 0 ]; then
        echo "✅ HPA successfully scaled down to ${CURRENT_REPLICAS} replica(s)."
        SCALED_DOWN=true
        break
    fi
    echo "Wait ${i}/${MAX_WAIT_SCALE}: Currently at ${CURRENT_REPLICAS} replicas. Waiting for scale down..."
    sleep 5
done

if [ "$SCALED_DOWN" = false ]; then
    echo "❌ ERROR: HPA failed to scale down to 1 or 0 within timeout. Scale-to-Zero logic might be broken."
    exit 1
fi


echo "============================================================================="
echo "▶️ STEP 4: Reconcile HPA and Variant (VA) Objectives"
echo "============================================================================="
HPA_MAX=$(oc get hpa "${HPA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.maxReplicas}')
VA_NAME=$(oc get va -n "${NAMESPACE}" -o jsonpath="{.items[0].metadata.name}")
VA_MAX=$(oc get va "${VA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.maxReplicas}')

echo "Detected HPA maxReplicas: ${HPA_MAX}"
echo "Detected VA maxReplicas: ${VA_MAX}"

if [ "${HPA_MAX}" != "${VA_MAX}" ]; then
    echo "⚠️ Discrepancy detected between HPA and VA! Patching Variant to align with HPA..."
    oc patch va "${VA_NAME}" -n "${NAMESPACE}" --type=merge -p "{\"spec\": {\"maxReplicas\": ${HPA_MAX}}}"
    echo "✅ Variant patched successfully."
else
    echo "✅ HPA and Variant maxReplicas are perfectly aligned."
fi


echo "============================================================================="
echo "▶️ STEP 5: Execute Benchmark"
echo "============================================================================="

if [ "$WORKLOAD_PROFILE" == "sharegpt" ]; then
    echo "Copying local sharegpt_data.jsonl to cluster PVC (/requests)..."
    DATA_ACCESS_POD=$(oc get pod -l role=llm-d-benchmark-data-access -n "$NAMESPACE" --no-headers -o 'jsonpath={.items[0].metadata.name}')
    if [ -n "$DATA_ACCESS_POD" ]; then
        oc cp "$REPO_ROOT/workload/profiles/guidellm/sharegpt_data.jsonl" "$NAMESPACE/$DATA_ACCESS_POD:/requests/sharegpt_data.jsonl"
        echo "✅ Dataset file successfully copied to the cluster."
    else
        echo "⚠️  Warning: Could not find data-access pod to upload dataset!"
    fi
fi

echo "Triggering GuideLLM load generator..."
cd "$REPO_ROOT" || exit 1
# COMMENTED OUT FOR DIRECT HPA PATCHING
# ./run.sh -l guidellm -w "$WORKLOAD_PROFILE" -p "$NAMESPACE" -m "$MODEL" -c "$SCENARIO"

echo "Benchmark standup complete. Bypassing execution for manual patching."
exit 0
