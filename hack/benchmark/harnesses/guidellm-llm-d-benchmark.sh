#!/usr/bin/env bash

echo Using experiment result dir: "$LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR"
mkdir -p "$LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR"

# Upgrade guidellm to a commit that supports the 'replay' profile when the
# harness image's bundled version does not.  The commit is supplied by the
# caller via LLMDBENCH_GUIDELLM_REPLAY_COMMIT (set in the WVA Makefile as
# GUIDELLM_REPLAY_COMMIT).  Set it to empty to skip the upgrade entirely once
# an official PyPI release with replay support is available.
_GUIDELLM_COMMIT="${LLMDBENCH_GUIDELLM_REPLAY_COMMIT:-}"
if [ -n "${_GUIDELLM_COMMIT}" ] && \
   ! python3 -c "from guidellm.benchmark.profiles import replay" 2>/dev/null; then
  echo "Upgrading guidellm to support replay profile (commit ${_GUIDELLM_COMMIT})..."
  pip install --quiet --upgrade \
    "git+https://github.com/vllm-project/guidellm.git@${_GUIDELLM_COMMIT}" \
    >> "${LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR}/guidellm_upgrade.log" 2>&1 \
    && echo "guidellm upgrade complete." \
    || { echo "guidellm upgrade failed; see guidellm_upgrade.log"; exit 1; }
fi

pushd "${LLMDBENCH_RUN_WORKSPACE_DIR}/profiles/guidellm" > /dev/null 2>&1
_WORKLOAD_FILE="${LLMDBENCH_RUN_WORKSPACE_DIR}/profiles/guidellm/${LLMDBENCH_RUN_EXPERIMENT_HARNESS_WORKLOAD_NAME}"

# Resolve a REPLACE_ENV_<VAR> placeholder to the named env var's value.
_resolve_env() {
  local val="$1"
  if [[ "${val}" == REPLACE_ENV_* ]]; then
    local envvar="${val#REPLACE_ENV_}"
    echo "${!envvar}"
  else
    echo "${val}"
  fi
}

_TARGET=$(_resolve_env "$(yq -r .target "${_WORKLOAD_FILE}")")
_MAX_SECONDS=$(yq -r '.max_seconds // empty' "${_WORKLOAD_FILE}")
_MAX_SECONDS_ARG=${_MAX_SECONDS:+--max-seconds ${_MAX_SECONDS}}
# Extract profile as JSON so guidellm's parse_arguments correctly deserialises it.
# The YAML 'profile:' field is a dict (e.g. {kind: replay}); passing it as a JSON
# string ensures it survives the CLI→Pydantic path without defaulting to SweepProfile.
_PROFILE_JSON=$(python3 -c "
import yaml, json, sys
data = yaml.safe_load(open('${_WORKLOAD_FILE}'))
p = data.get('profile')
sep = (',', ':')
if isinstance(p, dict):
    print(json.dumps(p, separators=sep))
elif isinstance(p, str):
    print(json.dumps({'kind': p}, separators=sep))
" 2>/dev/null)
_PROFILE_ARG=${_PROFILE_JSON:+--profile ${_PROFILE_JSON}}
export LLMDBENCH_HARNESS_ARGS="--target ${_TARGET} --scenario ${_WORKLOAD_FILE} ${_PROFILE_ARG} ${_MAX_SECONDS_ARG} --output-path ${LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR}/results.json --disable-progress"

# Start metrics collection in background if enabled
if [[ "${LLMDBENCH_VLLM_COMMON_METRICS_SCRAPE_ENABLED:-false}" == "true" ]]; then
  echo "Starting metrics collection..."
  /usr/local/bin/collect_metrics.sh start >> $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/metrics_collection.log 2>&1 &
  METRICS_COLLECTOR_PID=$!
  echo "Metrics collector started with PID: $METRICS_COLLECTOR_PID"
  echo "Metrics collection logs: $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/metrics_collection.log"
fi

start=$(date +%s.%N)
guidellm benchmark $LLMDBENCH_HARNESS_ARGS > >(tee -a $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/stdout.log) 2> >(tee -a $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/stderr.log >&2)
export LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC=$?
stop=$(date +%s.%N)

# Stop metrics collection
if [[ "${LLMDBENCH_VLLM_COMMON_METRICS_SCRAPE_ENABLED:-false}" == "true" ]] && [[ -n "${METRICS_COLLECTOR_PID:-}" ]]; then
  echo "Stopping metrics collection..."
  /usr/local/bin/collect_metrics.sh stop >> $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/metrics_collection.log 2>&1
  wait $METRICS_COLLECTOR_PID 2>/dev/null || true

  # Process collected metrics
  echo "Processing collected metrics..."
  /usr/local/bin/collect_metrics.sh process >> $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/metrics_collection.log 2>&1

  echo "Metrics collection complete. Check metrics_collection.log for details."
fi

export LLMDBENCH_HARNESS_START=$(date -d "@${start}" --iso-8601=seconds)
export LLMDBENCH_HARNESS_STOP=$(date -d "@${stop}" --iso-8601=seconds)
export LLMDBENCH_HARNESS_DELTA=PT$(echo "$stop - $start" | bc)S
export LLMDBENCH_HARNESS_VERSION=$(guidellm --version)

# Write run metadata to a file so the analyzer can read it.
# Environment variables exported here are lost when this subshell exits,
# so the file serves as the handoff mechanism to the analysis phase.
cat > "$LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/run_metadata.yaml" <<METADATA
harness_start: "${LLMDBENCH_HARNESS_START}"
harness_stop: "${LLMDBENCH_HARNESS_STOP}"
harness_delta: "${LLMDBENCH_HARNESS_DELTA}"
harness_args: "${LLMDBENCH_HARNESS_ARGS}"
harness_version: "${LLMDBENCH_HARNESS_VERSION}"
harness_name: "${LLMDBENCH_HARNESS_NAME:-guidellm}"
harness_workload: "${LLMDBENCH_RUN_EXPERIMENT_HARNESS_WORKLOAD_NAME:-}"
harness_rc: "${LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC}"
model: "${LLMDBENCH_DEPLOY_CURRENT_MODEL:-}"
endpoint_url: "${LLMDBENCH_HARNESS_STACK_ENDPOINT_URL:-}"
namespace: "${LLMDBENCH_VLLM_COMMON_NAMESPACE:-}"
METADATA
echo "Run metadata written to $LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR/run_metadata.yaml"

# If benchmark harness returned with an error, exit here
if [[ $LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC -ne 0 ]]; then
  echo "Harness returned with error $LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC"
  exit $LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC
fi
echo "Harness completed successfully."

exit $LLMDBENCH_RUN_EXPERIMENT_HARNESS_RC
