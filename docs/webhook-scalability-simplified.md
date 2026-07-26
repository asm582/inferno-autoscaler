# Webhook Scalability: Simplified

## Quick Answer to Your Questions

**Can webhooks scale?** ✅ Yes, horizontally to any number of replicas

**Is this a SPOF?** ⚠️ No, with 3 replicas + pod anti-affinity (implemented)

**What's the fallback with `failurePolicy: Ignore`?** 🔴 There is NO fallback—evictions proceed with random pod selection (not queue-aware)

---

## Your Implementation: 3-Replica HA Setup

```yaml
# What you have:
replicas: 3
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        labelSelector:
          matchLabels:
            app: inferno-webhook
        topologyKey: kubernetes.io/hostname  # No two pods on same node

podDisruptionBudget:
  minAvailable: 1  # At least 1 pod must always be running
```

**This is sufficient for production.** No need for zone-level complexity.

---

## Scalability Characteristics

### Load Distribution

Each Kubernetes API server independently load-balances webhook requests across all 3 replicas using the webhook Service (kube-proxy round-robin).

```
API Server 1 ──┐
API Server 2 ──┼─→ Webhook Service (ClusterIP)
API Server 3 ──┤
               ├─→ Replica 1 (pod)
               ├─→ Replica 2 (pod)
               └─→ Replica 3 (pod)
```

### Throughput Per Replica

Your webhook per-pod latency: ~75ms

```
Per replica: 200 API slots ÷ 75ms = 2.6 evictions/sec
3 replicas total: ~8 evictions/sec
```

For KEDA inference workloads (typical 1-2 pod evictions per scale-down event), this is **more than sufficient**.

### Failure Tolerance

| Scenario | Impact |
|----------|--------|
| 1 pod fails | 2 pods remain → minimal latency increase |
| 1 pod node maintenance | PDB ensures 1 pod stays running |
| 2 pods fail | Still 1 pod operational → cluster continues |
| All 3 pods down | Webhook unavailable → depends on failurePolicy |

---

## failurePolicy: Ignore = Random Fallback

**Critical: When webhook is down with `failurePolicy: Ignore`:**

```
Normal: Webhook patches costs → HPA evicts lowest-cost pod ✅ (queue-aware)

Failed:  Webhook skipped → no patching → HPA evicts random pod ❌ (no queue-awareness)
```

**There is NO explicit fallback strategy.** HPA just proceeds without the webhook, which means random pod selection.

**Accept this trade-off:** You get cluster availability but lose queue-aware pod selection during webhook outages. For inference workloads where outages are rare (3 replicas = 99%+ uptime), this is acceptable.

---

## Simple HA Deployment Pattern

Your implementation already follows industry best practices:

```yaml
# ✅ Already implemented:
- 3 replicas (handles failures)
- Pod anti-affinity on hostname (no two pods on same node)
- PDB minAvailable: 1 (survives node maintenance)
- failurePolicy: Ignore (cluster stays operational)
- timeoutSeconds: 5 (doesn't block cluster)

# ✅ Recommended additions:
- Readiness probes (detect pod failures)
- Liveness probes (restart hung pods)
- Pod resource requests/limits (predictable scheduling)
- Monitoring alerts on webhook unavailability
```

---

## Bottom Line

**Your 3-replica setup is production-ready.** No zone-level complexity needed for inference workloads. It provides:

- ✅ Handles any single pod failure
- ✅ Survives node maintenance  
- ✅ ~8 evictions/sec throughput (plenty for KEDA)
- ✅ Automatic failover with kube-proxy
- ⚠️ During outage: random eviction (acceptable for gentle scale-down)

Keep it simple. It works.
