# Webhook Deployment on OpenShift

This guide covers OpenShift-specific configuration and requirements for deploying the queue-aware pod selection webhook.

## Key Differences from Vanilla Kubernetes

### Security Context Constraint (SCC)

OpenShift enforces **Security Context Constraints** by default. Unlike Kubernetes, this is not optional.

**Default behavior:**
- Webhook pods run under `restricted-v2` SCC (OpenShift 4.12+) or `restricted` (4.10-4.11)
- UID automatically injected (non-zero, non-root)
- All capabilities dropped except `CAP_NET_BIND_SERVICE`
- No `allowPrivilegeEscalation`
- `readOnlyRootFilesystem` may be enforced

**Critical:** The deployment manifests include:
```yaml
securityContext: {}  # IMPORTANT: Empty, not omitted
```

This **allows OpenShift's SCC to inject security settings** rather than pod requesting them explicitly.

### Service Account SCC Binding

The webhook's ServiceAccount **must** be bound to an SCC:

```bash
# Applied automatically via rbac-openshift.yaml:
oc adm policy add-scc-to-user system:openshift:scc:nonroot \
  -z inferno-webhook \
  -n inferno-system
```

If this binding is missing:
```
Error: securitycontextconstraints.security.openshift.io "restricted" not usable by 
service account "inferno-webhook"
```

## Deployment Steps for OpenShift

### 1. Create Namespace

```bash
oc new-project inferno-system
```

### 2. Deploy RBAC (OpenShift-specific)

```bash
# Use OpenShift RBAC with SCC bindings
oc apply -f config/webhook/rbac-openshift.yaml
```

This creates:
- ServiceAccount `inferno-webhook`
- ClusterRole with SCC read permissions
- ClusterRoleBinding for RBAC
- **ClusterRoleBinding for SCC `nonroot` access** (OpenShift-specific)

### 3. Deploy Webhook Service & Deployment

```bash
oc apply -f config/webhook/service.yaml
oc apply -f config/webhook/deployment.yaml
```

The deployment uses `securityContext: {}` to allow SCC injection.

### 4. Register MutatingWebhookConfiguration

```bash
oc apply -f config/webhook/mutatingwebhook.yaml
```

### 5. Verify Deployment

```bash
# Check webhook pods are running
oc get pods -n inferno-system -l app=inferno-webhook

# Check assigned SCC (should be nonroot or restricted-v2)
oc describe pod <webhook-pod> -n inferno-system | grep scc

# Check service account SCC permissions
oc describe sa inferno-webhook -n inferno-system
```

## OpenShift-Specific Considerations

### 1. SCC Re-evaluation After Mutation

**Critical Gotcha:** When the webhook patches a pod's annotations:

1. Initial SCC evaluation → assigns UID/fsGroup to webhook pod
2. Webhook patches pod annotations
3. OpenShift's SCC admission **re-runs automatically**
4. If webhook mutation conflicts with SCC, pod may be rejected

**Mitigation:** The webhook only patches pod **annotations** (`controller.kubernetes.io/pod-deletion-cost`), not security-sensitive fields. This avoids SCC re-evaluation conflicts.

### 2. Pod Security Admission Labels

On OpenShift 4.11+, namespaces are automatically labeled with Pod Security Admission levels:
- `pod-security.kubernetes.io/enforce: restricted`
- `pod-security.kubernetes.io/audit: restricted`
- `pod-security.kubernetes.io/warn: restricted`

These are **informational only** for the webhook; they don't affect webhook operation.

### 3. Certificate Management

For TLS certificates, use OpenShift's service-ca operator:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: inferno-webhook
  namespace: inferno-system
  annotations:
    # This annotation tells OpenShift service-ca to inject CA bundle
    service.beta.openshift.io/inject-cabundle: "true"
spec:
  ports:
  - name: webhook
    port: 443
    targetPort: 9443
```

OpenShift automatically creates and rotates TLS certificates in the secret `webhook-certs`.

### 4. Webhook Image Compatibility

The webhook image **must be built to run as non-root**:

```dockerfile
FROM golang:1.21 AS builder
RUN CGO_ENABLED=0 GOOS=linux go build -o webhook ./cmd/webhook

# Final image: must run as non-root
FROM gcr.io/distroless/base:nonroot
# distroless/base:nonroot runs as UID 65532 by default
COPY --from=builder /workspace/webhook /webhook
ENTRYPOINT ["/webhook"]
```

The provided webhook binary is compatible (uses `gcr.io/distroless/base:nonroot`).

### 5. Version-Specific Requirements

| OpenShift Version | SCC Default | Key Changes |
|-------------------|-------------|------------|
| 4.10 | `restricted` (v1) | More lenient; older policy |
| 4.11 | `restricted` (v1) + optional `restricted-v2` | Opt-in v2; test both |
| 4.12+ | `restricted-v2` default | Stricter: no priv escalation, all caps dropped |
| 4.14+ | `restricted-v2` enforced | All new pods use v2; backward compat warnings |

**Action:** Test webhook on 4.12+ (stricter SCC) to ensure compatibility.

## Troubleshooting

### Pod Not Starting: SCC Error

```
Error: securitycontextconstraints.security.openshift.io "restricted" not usable
```

**Fix:**
```bash
# Verify SCC binding exists
oc get clusterrolebindings | grep webhook

# If missing, bind service account to SCC
oc adm policy add-scc-to-user system:openshift:scc:nonroot \
  -z inferno-webhook \
  -n inferno-system
```

### Webhook Pod Running But UID Unexpected

```bash
# Check assigned UID
oc describe pod <webhook-pod> -n inferno-system | grep UID

# Should see UID in range like 1000700000:1000799999 (SCC-injected)
# NOT 65534 (specified) or 0 (root)
```

If UID is not SCC-injected:
```bash
# Check securityContext in pod spec
oc get pod <webhook-pod> -n inferno-system -o yaml | grep -A5 securityContext

# Should be {} or null, not {runAsUser: 65534}
```

### Webhook Not Intercepting Evictions

```bash
# Verify MutatingWebhookConfiguration is registered
oc get mutatingwebhookconfigurations inferno-pod-selection

# Check webhook service is reachable
oc exec -it <webhook-pod> -n inferno-system -- curl -v https://localhost:9443/health

# Check webhook logs
oc logs -f deployment/inferno-webhook -n inferno-system
```

### Certificate Issues

```bash
# Check if service-ca injected CA bundle
oc get secret webhook-certs -n inferno-system -o yaml | grep ca.crt

# If missing, manually inject
oc annotate service inferno-webhook \
  service.beta.openshift.io/inject-cabundle=true \
  -n inferno-system --overwrite
```

## Performance Notes

On OpenShift, the webhook experiences minimal overhead from SCC enforcement:
- SCC evaluation: ~1ms per pod (negligible)
- No performance degradation vs. vanilla Kubernetes
- UID injection is transparent to webhook

## Production Deployment Checklist

- [ ] Namespace created: `oc new-project inferno-system`
- [ ] RBAC applied: `oc apply -f config/webhook/rbac-openshift.yaml`
- [ ] Service deployed: `oc apply -f config/webhook/service.yaml`
- [ ] Deployment created: `oc apply -f config/webhook/deployment.yaml`
- [ ] MutatingWebhookConfiguration registered: `oc apply -f config/webhook/mutatingwebhook.yaml`
- [ ] Pods running: `oc get pods -n inferno-system`
- [ ] SCC binding verified: `oc describe sa inferno-webhook -n inferno-system`
- [ ] Webhook intercepting: Check logs for eviction events
- [ ] Monitoring configured: Scrape webhook metrics endpoint

## Related Documentation

- [OpenShift Security Context Constraints](https://docs.openshift.com/container-platform/latest/authentication/managing-security-context-constraints.html)
- [Pod Security Admission on OpenShift](https://docs.openshift.com/container-platform/latest/security/pods/pod-security-standards.html)
- [Webhooks on OpenShift](https://docs.openshift.com/container-platform/latest/architecture/admission-plug-ins/admission-plug-ins-api.html)
