# Queue-Aware Pod Selection for Graceful Eviction

**Status:** Proposal  
**Date:** 2026-07-25  
**Author:** Inferno Team  
**Related Issue:** llm-d/llm-d#XXXX

## Problem Statement

When Kubernetes HPA scales down inference workloads, it randomly selects pods for eviction. This can lead to:

- **Request loss**: Evicting pods with pending requests in their queue
- **Poor latency**: Forcing clients to resubmit evicted requests
- **Inefficiency**: Not preferring underutilized pods for deletion

**Example:**
```
3 pods running:
  pod-0: 0 pending requests (idle)
  pod-1: 45 pending requests
  pod-2: 50 pending requests

HPA scales down 3→2 randomly
→ 50% chance it evicts pod-1 or pod-2 (both busy)
→ Requests lost, clients affected
```

## Solution: Admission Webhook for Pod Deletion Cost

Intercept Kubernetes eviction requests with a webhook that:

1. **Queries EPP** for real-time per-pod metrics (queue depth, running requests)
2. **Calculates deletion cost** for each pod using formula:
   ```
   cost = (queue_depth × 10) + (running_requests × 5) - 100
   ```
3. **Patches pods** with `controller.kubernetes.io/pod-deletion-cost` annotation
4. **Allows eviction** to proceed → HPA uses annotations to pick lowest-cost pod

**Result:** HPA always evicts the safest pod first (lowest queue depth).

## Architecture

```
┌──────────────────────────────────────────────────────┐
│ Kubernetes Cluster                                   │
├──────────────────────────────────────────────────────┤
│                                                      │
│  ┌─ Inference Workload                              │
│  │  └─ 3 pods (pod-0, pod-1, pod-2)                 │
│  │                                                  │
│  ├─ EPP (Endpoint Picker)                           │
│  │  └─ Provides per-pod metrics via HTTP API        │
│  │                                                  │
│  ├─ HPA (Horizontal Pod Autoscaler)                 │
│  │  └─ Watches total queue depth                    │
│  │  └─ Decides replica count                        │
│  │  └─ Respects pod deletion costs                  │
│  │                                                  │
│  └─ Inferno Pod Selection Webhook (NEW)             │
│     └─ 2-3 replicas for HA                          │
│     └─ Intercepts eviction requests                 │
│     └─ Queries EPP → Patches pods → Admits          │
│                                                      │
└──────────────────────────────────────────────────────┘
```

## Implementation Plan

### Phase 1: Webhook Service (inferno-autoscaler)

**Create new directory structure:**
```
inferno-autoscaler/
├─ cmd/
│  └─ webhook/
│     └─ main.go                    # Webhook server entry point
│
├─ internal/
│  ├─ webhook/
│  │  ├─ handler.go                 # Admission webhook request handler
│  │  ├─ server.go                  # TLS server, cert rotation
│  │  └─ epp.go                     # Query EPP for pod metrics
│  │
│  └─ podselection/
│     ├─ cost.go                    # Deletion cost calculation
│     └─ annotator.go               # Patch pod annotations
│
├─ Dockerfile                        # Multi-stage: webhook builder
└─ config/
   └─ webhook/
      ├─ deployment.yaml            # 2-3 replicas, anti-affinity
      ├─ service.yaml               # ClusterIP for webhook
      ├─ serviceaccount.yaml        # RBAC identity
      ├─ clusterrole.yaml           # Permissions to patch pods
      ├─ clusterrolebinding.yaml    # Bind role to account
      └─ mutatingwebhook.yaml       # MutatingWebhookConfiguration
```

### Phase 2: Implementation Details

#### 2.1 Webhook Handler Logic

```go
// Handle incoming AdmissionReview request
func (h *Handler) Handle(ctx context.Context, review *admissionv1.AdmissionReview) {
  // 1. Parse eviction request
  eviction := parseEvictionRequest(review)
  if eviction == nil {
    return admit(review)  // Not an eviction, allow
  }
  
  // 2. Query EPP for current pod metrics
  pods := listPods(namespace)
  metrics := queryEPP(pods)  // [pod-name] → {queue, running}
  
  // 3. Calculate deletion costs
  for pod, metric := range metrics {
    cost := calculateDeletionCost(metric.queue, metric.running)
    patch(pod, cost)  // Set annotation
  }
  
  // 4. Allow eviction
  return admit(review)
}
```

#### 2.2 Deletion Cost Formula

```go
func calculateDeletionCost(queueDepth, runningRequests int) int {
  // Higher cost = higher priority to keep (lower priority to delete)
  // Lower cost = lower priority to keep (higher priority to delete)
  
  if queueDepth == 0 && runningRequests == 0 {
    return -100  // Idle pod → delete first
  }
  
  // Weighted: queue is more expensive to lose than in-flight
  cost := (queueDepth * 10) + (runningRequests * 5) - 100
  
  if cost > 1000 {
    return 1000  // Cap to prevent overflow
  }
  
  return cost
}
```

#### 2.3 EPP Integration

```go
// Query EPP for per-pod metrics
func queryEPP(pods []Pod) map[string]PodMetrics {
  // GET http://epp:8080/endpoints
  // Returns: [{pod: "pod-0", WaitingQueueSize: 0, RunningRequestsSize: 0}, ...]
  
  metrics := make(map[string]PodMetrics)
  resp := httpGet("http://epp:8080/endpoints")
  
  for _, ep := range resp.Endpoints {
    metrics[ep.Pod] = PodMetrics{
      queue:    ep.WaitingQueueSize,
      running:  ep.RunningRequestsSize,
    }
  }
  
  return metrics
}
```

#### 2.4 Pod Annotation Patching

```go
func patchPodAnnotation(ctx context.Context, pod *Pod, cost int) error {
  // Use Kubernetes client to patch pod annotation
  // controller.kubernetes.io/pod-deletion-cost = cost
  
  patch := map[string]interface{}{
    "metadata": map[string]interface{}{
      "annotations": map[string]string{
        "controller.kubernetes.io/pod-deletion-cost": strconv.Itoa(cost),
      },
    },
  }
  
  _, err := kubeClient.CoreV1().Pods(pod.Namespace).Patch(
    ctx, pod.Name, types.MergePatchType, patch)
  
  return err
}
```

### Phase 3: Kubernetes Configuration

#### 3.1 MutatingWebhookConfiguration

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: inferno-pod-selection
webhooks:
- name: pod-selection.inferno.dev
  clientConfig:
    service:
      name: inferno-webhook
      namespace: default
      path: "/mutate"
    caBundle: <cert-manager-injected>
  
  rules:
  - operations: ["CREATE"]
    apiGroups: [""]
    apiVersions: ["v1"]
    resources: ["pods/eviction"]
  
  admissionReviewVersions: ["v1"]
  sideEffects: None
  failurePolicy: Ignore  # Don't block evictions if webhook fails
  timeoutSeconds: 5
  namespaceSelector:
    matchLabels:
      workload: inference  # Only target inference namespace
```

#### 3.2 Deployment with HA

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inferno-webhook
spec:
  replicas: 2  # Minimum 2 for HA
  selector:
    matchLabels:
      app: inferno-webhook
  
  template:
    metadata:
      labels:
        app: inferno-webhook
    
    spec:
      serviceAccountName: inferno-webhook
      
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchExpressions:
                - key: app
                  operator: In
                  values: [inferno-webhook]
              topologyKey: kubernetes.io/hostname
      
      containers:
      - name: webhook
        image: ghcr.io/llm-d/inferno-autoscaler:webhook-latest
        args: ["webhook"]
        
        ports:
        - name: webhook
          containerPort: 9443
        
        env:
        - name: EPP_URL
          value: "http://epp:8080"
        - name: NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        
        volumeMounts:
        - name: webhook-certs
          mountPath: /etc/webhook/certs
          readOnly: true
        
        livenessProbe:
          httpGet:
            path: /health
            port: 9443
            scheme: HTTPS
          initialDelaySeconds: 10
          periodSeconds: 10
        
        readinessProbe:
          httpGet:
            path: /ready
            port: 9443
            scheme: HTTPS
          initialDelaySeconds: 5
          periodSeconds: 5
        
        resources:
          requests:
            cpu: 100m
            memory: 128Mi
          limits:
            cpu: 500m
            memory: 512Mi
      
      volumes:
      - name: webhook-certs
        secret:
          secretName: inferno-webhook-certs
---
apiVersion: v1
kind: Service
metadata:
  name: inferno-webhook
spec:
  selector:
    app: inferno-webhook
  ports:
  - port: 443
    targetPort: 9443
    name: webhook
  type: ClusterIP
```

#### 3.3 RBAC

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: inferno-webhook
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: inferno-webhook
rules:
# List and get pods
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list", "get"]

# Patch pod annotations (for deletion cost)
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["patch"]
  resourceNames: []  # All pods in namespace

# Read configmaps for configuration
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
  resourceNames: ["inferno-webhook-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: inferno-webhook
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: inferno-webhook
subjects:
- kind: ServiceAccount
  name: inferno-webhook
  namespace: default
```

### Phase 4: Testing

#### 4.1 Unit Tests

```go
// Test deletion cost calculation
func TestCalculateDeletionCost(t *testing.T) {
  tests := []struct {
    queue, running int
    expectedCost   int
  }{
    {0, 0, -100},        // Idle
    {1, 0, -90},         // Almost idle
    {10, 2, 200},        // Loaded
    {50, 5, 575},        // Very loaded
  }
  
  for _, tt := range tests {
    cost := calculateDeletionCost(tt.queue, tt.running)
    assert.Equal(t, tt.expectedCost, cost)
  }
}
```

#### 4.2 Integration Tests

```go
// Test webhook intercepts eviction correctly
func TestWebhookInterceptsEviction(t *testing.T) {
  // 1. Create 3 test pods with different queue depths
  // 2. Send eviction request via webhook
  // 3. Verify pods are patched with correct costs
  // 4. Verify eviction is admitted
}

// Test EPP integration
func TestEPPQueryMetrics(t *testing.T) {
  // Mock EPP server
  // Query per-pod metrics
  // Verify metrics are correctly parsed
}
```

#### 4.3 E2E Testing

```bash
# Manual test steps:
1. Deploy webhook and HPA to test cluster
2. Create inference workload with 3 pods
3. Verify pods are healthy
4. Trigger scale-down event
5. Verify webhook intercepts eviction
6. Verify webhook patches pods with costs
7. Verify HPA evicts lowest-cost pod
8. Verify no requests lost during eviction
```

### Phase 5: Deployment & Monitoring

#### 5.1 Build & Push

```dockerfile
# Dockerfile multi-stage build
FROM golang:1.21 AS builder

WORKDIR /workspace
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o webhook ./cmd/webhook

FROM gcr.io/distroless/base:nonroot

COPY --from=builder /workspace/webhook /webhook

ENTRYPOINT ["/webhook"]
```

#### 5.2 Monitoring Metrics

Export to Prometheus:
```go
// Webhook metrics
webhookRequestsTotal     // Total eviction requests intercepted
webhookRequestsDuration  // Latency of webhook processing
podCostsUpdated         // Number of pods patched per run
eppQueryDuration        // Time to query EPP
eppQueryErrors          // Failures to reach EPP
```

#### 5.3 Alerts

```yaml
# Alert: Webhook latency too high
- alert: InfernoWebhookLatency
  expr: webhookRequestsDuration > 1.0
  for: 5m
  annotations:
    summary: "Webhook latency high"

# Alert: Webhook not admitting evictions
- alert: InfernoWebhookFailures
  expr: rate(webhookRequestsTotal{status="denied"}[5m]) > 0
  for: 5m
  annotations:
    summary: "Webhook is rejecting evictions"

# Alert: EPP is unreachable
- alert: InfernoWebhookEPPDown
  expr: rate(eppQueryErrors[5m]) > 0.1
  for: 5m
  annotations:
    summary: "Webhook cannot reach EPP"
```

## Timeline & Effort

| Phase | Task | Effort | Timeline |
|-------|------|--------|----------|
| 1 | Webhook server scaffolding | 2 days | Week 1 |
| 2 | Core logic + EPP integration | 3 days | Week 1-2 |
| 3 | K8s config + RBAC | 1 day | Week 2 |
| 4 | Unit + integration tests | 2 days | Week 2 |
| 5 | E2E testing on staging | 2 days | Week 3 |
| 6 | Monitoring + docs | 1 day | Week 3 |
| **Total** | | **11 days** | **3 weeks** |

## Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Webhook latency delays evictions | `timeoutSeconds: 5`, `failurePolicy: Ignore`, async patching |
| EPP unavailable | `failurePolicy: Ignore` (allow eviction without patch), fallback to random |
| Pod patch failures | Log and alert, next eviction will retry |
| High webhook request volume | Cache metrics, deduplicate patch requests |
| Certificate rotation fails | cert-manager + automated renewal |

## Success Criteria

- ✅ Webhook intercepts 100% of eviction requests
- ✅ Eviction latency increased by <100ms (SLO)
- ✅ Idle pods (queue=0) always selected first
- ✅ Zero request loss during eviction
- ✅ E2E test passes with 3+ pod scale-down
- ✅ Monitoring/alerts in place

## Future Enhancements

- Consider WVA coordinator as alternative transport mechanism (not interceptor)
- Support workload-specific cost formulas (configurable per model)
- Persist cost history for analytics
- Integration with PodDisruptionBudgets (PDB)
