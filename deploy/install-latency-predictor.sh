#!/bin/bash
set -e

# Default values
DEFAULT_NAMESPACE="llm-d-inference-scheduler"

# Parse arguments
NAMESPACE=${1:-$DEFAULT_NAMESPACE}
POOL_NAME=${2:-}

echo "Using Namespace: $NAMESPACE"

# Detect POOL_NAME if not provided
if [ -z "$POOL_NAME" ]; then
  echo "Attempting to detect InferencePool in namespace '$NAMESPACE'..."
  if ! kubectl get inferencepool -n "$NAMESPACE" &>/dev/null; then
     echo "Error: No InferencePool found in namespace '$NAMESPACE'. Please ensure llm-d is installed."
     exit 1
  fi
  
  POOL_NAME=$(kubectl get inferencepool -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  
  if [ -z "$POOL_NAME" ]; then
    echo "Error: Could not automatically detect InferencePool name. Please provide it as the second argument."
    echo "Usage: $0 [NAMESPACE] [POOL_NAME]"
    exit 1
  fi
  echo "Detected InferencePool: $POOL_NAME"
else
  echo "Using InferencePool: $POOL_NAME"
fi

# Images configuration
# Users likely need to build these or pull from a specific registry. 
# Defaults match the experimental guide/manifest.
export EPP_IMAGE=${EPP_IMAGE:-epp:experimental}
export TRAINING_IMAGE=${TRAINING_IMAGE:-latency_training:experimental}
export PREDICTION_IMAGE=${PREDICTION_IMAGE:-latency_prediction:experimental}

echo "Using Images:"
echo "  EPP:        $EPP_IMAGE"
echo "  Training:   $TRAINING_IMAGE"
echo "  Prediction: $PREDICTION_IMAGE"

export NAMESPACE
export POOL_NAME

# Template processing
TEMPLATE_FILE="deploy/manifests/latency-predictor.template.yaml"

if [ ! -f "$TEMPLATE_FILE" ]; then
    echo "Error: Template file $TEMPLATE_FILE not found."
    exit 1
fi

echo "Applying latency predictor manifests..."
envsubst < "$TEMPLATE_FILE" | kubectl apply -f -

echo "---------------------------------------------------"
echo "Latency Predictor deployment initiated."
echo "Verify configuration with:"
echo "  kubectl get pods -n $NAMESPACE -l app=$POOL_NAME-epp"
echo "  kubectl get svc -n $NAMESPACE $POOL_NAME-epp"
