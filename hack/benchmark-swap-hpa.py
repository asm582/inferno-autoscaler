#!/usr/bin/env python3
"""Swap the WVA-managed HPA for a plain Saturation V1 HPA.

Usage:
    benchmark-swap-hpa.py <namespace> <kv-target> <queue-target> <min-replicas> <max-replicas> <scale-up-period-sec> <scale-down-period-sec>

Note: this script shells out to kubectl rather than using the Python kubernetes client
(kubernetes-client/python) to keep the dependency footprint small — the benchmark
environment already requires kubectl, so no extra install is needed.
"""

import argparse
import json
import subprocess
import sys

import yaml


def kubectl(*args, capture=False, check=True):
    return subprocess.run(
        ["kubectl", *args],
        check=check,
        capture_output=capture,
        text=True,
    )


def kubectl_jsonpath(*args, jsonpath, check=True):
    result = kubectl(*args, f"-o=jsonpath={jsonpath}", capture=True, check=check)
    return result.stdout.strip()


def step(n, total, msg):
    print(f"[{n}/{total}] {msg}")


def scale_wva_to_zero():
    step(1, 5, "Scaling WVA controller to 0...")
    ns = kubectl_jsonpath(
        "get", "deployment", "--all-namespaces",
        "-l", "app.kubernetes.io/name=llm-d-workload-variant-autoscaler",
        jsonpath="{.items[0].metadata.namespace}",
        check=False,
    )
    if not ns:
        print("    WVA controller not found; skipping")
        return
    name = kubectl_jsonpath(
        "get", "deployment", "-n", ns,
        "-l", "app.kubernetes.io/name=llm-d-workload-variant-autoscaler",
        jsonpath="{.items[0].metadata.name}",
    )
    kubectl("scale", "deployment", name, "--replicas=0", "-n", ns)
    print(f"    Scaled {name} to 0 in {ns}")


def discover_decode(ns):
    step(2, 5, "Discovering decode Deployment...")
    name = kubectl_jsonpath(
        "get", "variantautoscaling", "-n", ns,
        jsonpath="{.items[0].spec.scaleTargetRef.name}",
    )
    print(f"    Deployment: {name}")
    return name


def delete_wva_hpa(decode, ns):
    step(3, 5, f"Deleting WVA-managed HPA {decode}...")
    kubectl("delete", "hpa", decode, "-n", ns, "--ignore-not-found=true")


def patch_prometheus_adapter():
    step(4, 5, "Adding vLLM rules to WVA prometheus-adapter...")

    # Find the adapter the HPA actually talks to — the one registered for external metrics
    pa_ns = kubectl_jsonpath(
        "get", "apiservice", "v1beta1.external.metrics.k8s.io",
        jsonpath="{.spec.service.namespace}",
    )
    if not pa_ns:
        print("ERROR: no adapter registered for v1beta1.external.metrics.k8s.io", file=sys.stderr)
        sys.exit(1)
    print(f"    Adapter namespace: {pa_ns}")

    config_raw = kubectl_jsonpath(
        "get", "configmap", "prometheus-adapter", "-n", pa_ns,
        jsonpath=r"{.data.config\.yaml}",
    )
    cfg = yaml.safe_load(config_raw) or {}
    ext = cfg.setdefault("externalRules", [])
    existing = {r.get("name", {}).get("as") for r in ext}

    # Scoped by namespace+pod — no model_name (contains '/' which is invalid in k8s label values)
    vllm_rules = [
        {
            "seriesQuery": 'vllm:kv_cache_usage_perc{namespace!="",pod!=""}',
            "resources": {"overrides": {
                "namespace": {"resource": "namespace"},
                "pod":       {"resource": "pod"},
            }},
            "name": {"matches": "vllm:kv_cache_usage_perc", "as": "vllm_kv_cache_usage_perc"},
            "metricsQuery": "max by (<<.GroupBy>>) (max_over_time(vllm:kv_cache_usage_perc{<<.LabelMatchers>>}[1m]))",
        },
        {
            "seriesQuery": 'vllm:num_requests_waiting{namespace!="",pod!=""}',
            "resources": {"overrides": {
                "namespace": {"resource": "namespace"},
                "pod":       {"resource": "pod"},
            }},
            "name": {"matches": "vllm:num_requests_waiting", "as": "vllm_num_requests_waiting"},
            "metricsQuery": "max by (<<.GroupBy>>) (max_over_time(vllm:num_requests_waiting{<<.LabelMatchers>>}[1m]))",
        },
    ]

    added = [r for r in vllm_rules if r["name"]["as"] not in existing]
    if not added:
        print("    vLLM rules already present; skipping")
    else:
        ext.extend(added)
        patch = json.dumps({"data": {"config.yaml": yaml.dump(cfg, default_flow_style=False)}})
        kubectl("patch", "configmap", "prometheus-adapter", "-n", pa_ns, "--type=merge", "-p", patch)
        print(f"    Added {len(added)} rule(s)")
        kubectl("rollout", "restart", "deployment/prometheus-adapter", "-n", pa_ns)
        kubectl("rollout", "status", "deployment/prometheus-adapter", "-n", pa_ns, "--timeout=120s")


def apply_hpa(decode, ns, kv_target, queue_target, min_replicas, max_replicas,
              scale_up_period, scale_down_period):
    step(5, 5, "Applying Saturation V1 HPA...")
    hpa = {
        "apiVersion": "autoscaling/v2",
        "kind": "HorizontalPodAutoscaler",
        "metadata": {
            "name": decode,
            "namespace": ns,
            "annotations": {"llm-d.ai/managed-by": "saturation-v1-hpa"},
        },
        "spec": {
            "scaleTargetRef": {"apiVersion": "apps/v1", "kind": "Deployment", "name": decode},
            "minReplicas": int(min_replicas),
            "maxReplicas": int(max_replicas),
            "metrics": [
                {"type": "External", "external": {
                    "metric": {"name": "vllm_kv_cache_usage_perc"},
                    "target": {"type": "AverageValue", "averageValue": kv_target},
                }},
                {"type": "External", "external": {
                    "metric": {"name": "vllm_num_requests_waiting"},
                    "target": {"type": "AverageValue", "averageValue": queue_target},
                }},
            ],
            "behavior": {
                "scaleUp": {
                    "stabilizationWindowSeconds": 0,
                    "policies": [{"type": "Pods", "value": 1, "periodSeconds": int(scale_up_period)}],
                    "selectPolicy": "Max",
                },
                "scaleDown": {
                    "stabilizationWindowSeconds": 300,
                    "policies": [{"type": "Pods", "value": 1, "periodSeconds": int(scale_down_period)}],
                    "selectPolicy": "Min",
                },
            },
        },
    }
    subprocess.run(
        ["kubectl", "apply", "-f", "-"],
        input=yaml.dump(hpa),
        text=True,
        check=True,
    )
    print(f"\nDone. Verify with: kubectl get hpa {decode} -n {ns}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("namespace")
    parser.add_argument("kv_target")
    parser.add_argument("queue_target")
    parser.add_argument("min_replicas")
    parser.add_argument("max_replicas")
    parser.add_argument("scale_up_period")
    parser.add_argument("scale_down_period")
    args = parser.parse_args()

    scale_wva_to_zero()
    decode = discover_decode(args.namespace)
    delete_wva_hpa(decode, args.namespace)
    patch_prometheus_adapter()
    apply_hpa(decode, args.namespace, args.kv_target, args.queue_target,
              args.min_replicas, args.max_replicas,
              args.scale_up_period, args.scale_down_period)


if __name__ == "__main__":
    main()
