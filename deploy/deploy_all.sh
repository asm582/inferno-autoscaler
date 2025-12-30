#!/bin/bash
set -e

echo "=================================================="
echo "      Deploying Predictive Autoscaling System     "
echo "=================================================="

# 1. Prerequisites (Prometheus Adapter)
echo "[1/6] Installing Prometheus Adapter..."
if ! helm list -n monitoring | grep -q prometheus-adapter; then
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
    helm repo update
    helm install prometheus-adapter prometheus-community/prometheus-adapter \
        -n monitoring --create-namespace \
        -f deploy/verified_manifests/prometheus-adapter-values-no-tls.yaml
else
    echo "Prometheus Adapter already installed. upgrading..."
    helm upgrade prometheus-adapter prometheus-community/prometheus-adapter \
        -n monitoring \
        -f deploy/verified_manifests/prometheus-adapter-values-no-tls.yaml
fi

# 2. Deploy Gateway (Envoy)
echo "[2/6] Deploying Envoy Gateway..."
kubectl apply -f deploy/verified_manifests/standalone-gateway.yaml

# 3. Deploy Latency Predictor (EPP)
echo "[3/6] Deploying Latency Predictor (EPP)..."
kubectl apply -f deploy/verified_manifests/latency-predictor-manifests.yaml

# 4. Deploy WVA Controller (via Make)
echo "[4/6] Deploying WVA Controller..."
# Assuming we are in the project root
make deploy-wva-on-k8s

# 5. Deploy vLLM Application
echo "[5/6] Deploying vLLM Application..."
kubectl apply -f deploy/verified_manifests/vllm-deploy.yaml
kubectl apply -f deploy/verified_manifests/vllm-service.yaml


# 6. Deploy Autoscaling Resources (WVA CR + HPA)
echo "[6/6] Deploying Autoscaling Resources..."
kubectl apply -f deploy/verified_manifests/wva-va-test.yaml
kubectl apply -f deploy/verified_manifests/vllm-hpa.yaml

echo "=================================================="
echo "Deployment Complete!"
echo "Verify status with:"
echo "  kubectl get pods"
echo "  kubectl get va"
echo "  kubectl get hpa"
echo "=================================================="
