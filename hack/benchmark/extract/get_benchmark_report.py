import argparse
import json
import subprocess
import sys
import os
import urllib.parse
from typing import Dict, Any, Tuple, List
import datetime

try:
    import matplotlib.pyplot as plt
    import matplotlib.dates as mdates
    from matplotlib.backends.backend_pdf import PdfPages
    import yaml
except ImportError:
    print("❌ ERROR: Required package 'matplotlib' or 'pyyaml' is not installed.")
    print("Please run: pip install matplotlib pyyaml")
    sys.exit(1)

def check_privileges(token: str = None, server: str = None):
    """Verify the user is logged into OpenShift with sufficient monitoring exec privileges."""
    if token and server:
        print(f"Logging into OpenShift at {server}...")
        try:
            # Using --insecure-skip-tls-verify=true to avoid cert issues in basic CI setups
            subprocess.run(["oc", "login", f"--token={token}", f"--server={server}", "--insecure-skip-tls-verify=true"], capture_output=True, check=True)
            print("Successfully logged into OpenShift.")
        except subprocess.CalledProcessError as e:
            print("\n❌ ERROR: Failed to log into OpenShift with provided token and server.")
            print(f"Details: {e.stderr if e.stderr else 'Check your token and server URL.'}")
            sys.exit(1)

    print("Checking OpenShift privileges...")
    
    # Check if logged in
    try:
        subprocess.run(["oc", "whoami"], capture_output=True, check=True)
    except subprocess.CalledProcessError:
        print("\n❌ ERROR: You are not logged into OpenShift.")
        print("Please run: oc login --token=<your_token> --server=<cluster_url>")
        sys.exit(1)
        
    # Check if they can exec into the openshift-monitoring namespace
    try:
        result = subprocess.run(
            ["oc", "auth", "can-i", "create", "pods/exec", "-n", "openshift-monitoring"],
            capture_output=True, text=True, check=True
        )
        if "yes" not in result.stdout.lower():
            raise subprocess.CalledProcessError(1, "oc auth")
    except subprocess.CalledProcessError:
        print("\n❌ ERROR: Insufficient privileges.")
        print("You need 'cluster-admin' (or similar role able to exec into monitoring pods) to run this script.")
        print("Please log in as an admin or ask a cluster administrator for access.")
        sys.exit(1)

def query_prometheus(query: str, eval_time: float = None, user_workload: bool = False) -> Dict[str, Any]:
    encoded_query = urllib.parse.quote(query)
    
    namespace = "openshift-user-workload-monitoring" if user_workload else "openshift-monitoring"
    label_selector = "app.kubernetes.io/name=prometheus,prometheus=user-workload" if user_workload else "app.kubernetes.io/name=prometheus,prometheus=k8s"
    
    try:
        cmd_str = f"oc get pods -n {namespace} -l {label_selector} --no-headers | grep -v Terminating | awk '{{print $1}}' | head -n 1"
        pod_res = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, check=True)
        pod = pod_res.stdout.strip()
        if not pod:
            raise Exception("No running prometheus pod found")
    except Exception as e:
        print(f"Error finding running prometheus pod: {e}")
        pod = "prometheus-user-workload-0" if user_workload else "prometheus-k8s-0"
    
    if eval_time:
        url = f"http://localhost:9090/api/v1/query?query={encoded_query}&time={eval_time}"
    else:
        url = f"http://localhost:9090/api/v1/query?query={encoded_query}"
        
    cmd = [
        "oc", "exec", "-n", namespace, pod, "-c", "prometheus", "--",
        "curl", "-s", url
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        return json.loads(result.stdout)
    except Exception as e:
        print(f"Error querying Prometheus for query: {query}")
        print(e)
        return {}

def query_prometheus_range(query: str, start: float, end: float, step: str = "15s", user_workload: bool = False) -> Dict[str, Any]:
    encoded_query = urllib.parse.quote(query)
    
    namespace = "openshift-user-workload-monitoring" if user_workload else "openshift-monitoring"
    namespace = "openshift-user-workload-monitoring" if user_workload else "openshift-monitoring"
    label_selector = "app.kubernetes.io/name=prometheus,prometheus=user-workload" if user_workload else "app.kubernetes.io/name=prometheus,prometheus=k8s"
    
    try:
        cmd_str = f"oc get pods -n {namespace} -l {label_selector} --no-headers | grep -v Terminating | awk '{{print $1}}' | head -n 1"
        pod_res = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, check=True)
        pod = pod_res.stdout.strip()
        if not pod:
            raise Exception("No running prometheus pod found")
    except Exception as e:
        print(f"Error finding running prometheus pod: {e}")
        pod = "prometheus-user-workload-0" if user_workload else "prometheus-k8s-0"

    cmd = [
        "oc", "exec", "-n", namespace, pod, "-c", "prometheus", "--",
        "curl", "-s", f"http://localhost:9090/api/v1/query_range?query={encoded_query}&start={start}&end={end}&step={step}"
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        return json.loads(result.stdout)
    except Exception as e:
        print(f"Error querying Prometheus range for query: {query}")
        print(e)
        return {}

def get_node_gpus() -> Dict[str, str]:
    """Fetch all nodes and their GPU models (nvidia.com/gpu.product) directly from the cluster."""
    cmd = [
        "oc", "get", "nodes",
        "-o", "jsonpath={range .items[*]}{.metadata.name}{'\\t'}{.metadata.labels.nvidia\\.com/gpu\\.product}{'\\n'}{end}"
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        node_gpu_map = {}
        for line in result.stdout.strip().split('\n'):
            parts = line.split('\t')
            if len(parts) == 2:
                node, gpu = parts[0].strip(), parts[1].strip()
                if gpu:
                    node_gpu_map[node] = gpu
        return node_gpu_map
    except Exception as e:
        print(f"Warning: Could not fetch node GPU labels: {e}")
        return {}

def get_epp_config(namespace: str) -> str:
    """Fetch EPP configuration from ConfigMaps in the namespace."""
    cmd = ["oc", "get", "cm", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        for item in items:
            data = item.get("data", {})
            if "default-plugins.yaml" in data:
                return data["default-plugins.yaml"]
    except Exception as e:
        print(f"Warning: Could not fetch EPP config: {e}")
    return "EPP Config: Not Found"

def get_benchmark_config(namespace: str) -> str:
    """Fetch guidellm-profiles load generator configuration from ConfigMaps."""
    cmd = ["oc", "get", "cm", "guidellm-profiles", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        data = json.loads(result.stdout).get("data", {})
        # Depending on the profile name, there might be multiple files, let's just grab the first .yaml
        for key, value in data.items():
            if key.endswith(".yaml"):
                return f"Profile: {key}\n{value}"
        return str(data)
    except Exception as e:
        print(f"Warning: Could not fetch guidellm-profiles config: {e}")
    return "Benchmark Config: Not Found"

def get_autoscaling_config(namespace: str) -> Tuple[str, float]:
    """Fetch HPA and VA configuration (min/max replicas) from the namespace, and return the report string and the VA cost factor."""
    report = ""
    va_cost = 1.0 # Default cost multiplier if not found
    # Fetch VA
    cmd_va = ["oc", "get", "va", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd_va, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        if items:
            for item in items:
                name = item.get("metadata", {}).get("name", "Unknown")
                spec = item.get("spec", {})
                min_rep = spec.get("minReplicas", "N/A")
                max_rep = spec.get("maxReplicas", "N/A")
                va_cost = float(spec.get("variantCost", 1.0))
                report += f"Variant (VA) [{name}]: Min Replicas = {min_rep}, Max Replicas = {max_rep}, Cost Factor = {va_cost}\n"
        else:
            report += "Variant (VA): Not Found\n"
    except Exception as e:
        report += f"Warning: Could not fetch VA config: {e}\n"

    # Fetch HPA
    cmd_hpa = ["oc", "get", "hpa", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd_hpa, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        if items:
            for item in items:
                name = item.get("metadata", {}).get("name", "Unknown")
                spec = item.get("spec", {})
                min_rep = spec.get("minReplicas", "N/A")
                max_rep = spec.get("maxReplicas", "N/A")
                report += f"HorizontalPodAutoscaler (HPA) [{name}]: Min Replicas = {min_rep}, Max Replicas = {max_rep}\n"
                
                # Check for modern spec behavior or fallback to older annotation
                behavior = spec.get("behavior")
                if behavior:
                    scale_up = behavior.get("scaleUp", {})
                    scale_down = behavior.get("scaleDown", {})
                    s_up_win = scale_up.get("stabilizationWindowSeconds", "N/A")
                    s_up_pol = json.dumps(scale_up.get("policies", []))
                    s_dn_win = scale_down.get("stabilizationWindowSeconds", "N/A")
                    s_dn_pol = json.dumps(scale_down.get("policies", []))
                    report += f"  > ScaleUp:   Stabilization Window = {s_up_win}s | Policies = {s_up_pol}\n"
                    report += f"  > ScaleDown: Stabilization Window = {s_dn_win}s | Policies = {s_dn_pol}\n"
                else:
                    annotations = item.get("metadata", {}).get("annotations", {})
                    beh_str = annotations.get("autoscaling.alpha.kubernetes.io/behavior")
                    if beh_str:
                        try:
                            beh = json.loads(beh_str)
                            scale_up = beh.get("ScaleUp", {})
                            scale_down = beh.get("ScaleDown", {})
                            s_up_win = scale_up.get("StabilizationWindowSeconds", "N/A")
                            s_up_pol = json.dumps(scale_up.get("Policies", []))
                            s_dn_win = scale_down.get("StabilizationWindowSeconds", "N/A")
                            s_dn_pol = json.dumps(scale_down.get("Policies", []))
                            report += f"  > ScaleUp:   Stabilization Window = {s_up_win}s | Policies = {s_up_pol}\n"
                            report += f"  > ScaleDown: Stabilization Window = {s_dn_win}s | Policies = {s_dn_pol}\n"
                        except Exception as parse_e:
                            report += f"  > Behavior annotation found but could not be cleanly parsed.\n"
        else:
            report += "HorizontalPodAutoscaler (HPA): Not Found\n"
    except Exception as e:
        report += f"Warning: Could not fetch HPA config: {e}\n"

    return report, va_cost

def process_guidellm_results(filepath: str, step_seconds: int = 15) -> Tuple[List[datetime.datetime], List[int], List[int], List[int]]:
    with open(filepath, 'r') as f:
        results = json.load(f)
        
    successful_times = []
    failed_times = []
    incomplete_times = []
    
    for benchmark in results.get('benchmarks', []):
        for status in ['successful', 'incomplete', 'errored']:
            for req in benchmark.get('requests', {}).get(status, []):
                req_start = req.get('request_start_time', 0)
                if status == 'successful':
                    successful_times.append(req_start)
                elif status == 'errored':
                    failed_times.append(req_start)
                else: # incomplete
                    incomplete_times.append(req_start)
                    
    if not successful_times and not failed_times and not incomplete_times:
        return [], [], [], []
        
    min_time = min(successful_times + failed_times + incomplete_times)
    max_time = max(successful_times + failed_times + incomplete_times)
    
    # Create bins
    bins = {}
    current = min_time
    while current <= max_time + step_seconds:
        bins[current] = {'success': 0, 'fail': 0, 'incomplete': 0}
        current += step_seconds
        
    for t in successful_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['success'] += 1
            
    for t in failed_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['fail'] += 1

    for t in incomplete_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['incomplete'] += 1
            
    sorted_bins = sorted(bins.keys())
    x_times = [datetime.datetime.fromtimestamp(b) for b in sorted_bins]
    # converting count per 'step_seconds' into RPS (Rate Per Second)
    y_success = [bins[b]['success'] / step_seconds for b in sorted_bins]
    y_fail = [bins[b]['fail'] / step_seconds for b in sorted_bins]
    y_incomplete = [bins[b]['incomplete'] / step_seconds for b in sorted_bins]
    
    return x_times, y_success, y_fail, y_incomplete

def get_true_serving_capacity(filepath: str) -> str:
    """Parses results.json to find the highest achieved RPS before errors occur."""
    try:
        with open(filepath, 'r') as f:
            results = json.load(f)
            
        max_achieved_rps = 0.0
        details = []
        for b in results.get('benchmarks', []):
            rate = b.get('config', {}).get('strategy', {}).get('rate', 'N/A')
            metrics = b.get('metrics', {})
            
            achieved_rps = metrics.get('requests_per_second', {}).get('successful', {}).get('mean', 0)
            err_rps = metrics.get('requests_per_second', {}).get('errored', {}).get('mean', 0)
            tokens_sec = metrics.get('tokens_per_second', {}).get('successful', {}).get('mean', 0)
            
            details.append(f"Rate: {rate} RPS | Achieved: {achieved_rps:.2f} RPS | Errors: {err_rps:.2f} RPS | Tokens/s: {tokens_sec:.2f}")
            
            if err_rps == 0 and achieved_rps > max_achieved_rps:
                max_achieved_rps = achieved_rps
                
        # If there are no benchmarks with 0 errors, max_achieved_rps remains 0.0
        
        report = "True Serving Capacity Analysis (GuideLLM results.json)\n"
        report += "-" * 70 + "\n"
        report += "\n".join(details) + "\n"
        report += "-" * 70 + "\n"
        if max_achieved_rps > 0:
            report += f"=> Estimated True Serving Capacity: ~{max_achieved_rps:.2f} RPS (Highest successful rate with 0 errors)\n"
        else:
            report += "=> Estimated True Serving Capacity: Could not be determined (All runs had errors or 0 RPS)\n"
            
        report += "\n" + "-" * 70 + "\n"
        report += "Understanding GuideLLM Metrics:\n"
        report += "• Target Rate (RPS): The configured constant request rate the load generator attempts to send.\n"
        report += "• Achieved RPS: The actual rate of requests that completed successfully with a full response.\n"
        report += "• Error RPS: The rate of requests that failed (e.g., HTTP 5xx) or were strictly dropped/incomplete.\n"
        report += "• True Serving Capacity: Evaluated as the highest Achieved RPS recorded just before the system\n"
        report += "  reaches hardware saturation and begins generating Error RPS (dropped requests).\n"
        return report
        
    except Exception as e:
        return f"Could not determine True Serving Capacity: {e}\n"

def parse_guidellm_latencies(filepath: str) -> Tuple[List[str], List[float], List[float], List[float], List[float], List[float], List[float], List[float]]:
    """Returns (labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean) for all benchmarks."""
    labels = []
    ttft_mean, ttft_p99 = [], []
    itl_mean, itl_p99 = [], []
    tps_mean = []
    conc_mean = []
    req_lat_mean = []
    
    try:
        with open(filepath, 'r') as f:
            results = json.load(f)
        for i, benchmark in enumerate(results.get('benchmarks', [])):
            rate = benchmark.get('config', {}).get('strategy', {}).get('rate')
            if rate is not None:
                labels.append(f"{rate} RPS")
            else:
                labels.append(f"Run {i+1}")
            metrics = benchmark.get('metrics', {})
            
            # TTFT
            ttft = metrics.get('time_to_first_token_ms', {}).get('successful', {})
            ttft_mean.append(ttft.get('mean', 0))
            ttft_p99.append(ttft.get('percentiles', {}).get('p99', 0))
            
            # ITL
            itl = metrics.get('inter_token_latency_ms', {}).get('successful', {})
            itl_mean.append(itl.get('mean', 0))
            itl_p99.append(itl.get('percentiles', {}).get('p99', 0))
            
            # Token Throughput
            tps = metrics.get('tokens_per_second', {}).get('successful', {})
            tps_mean.append(tps.get('mean', 0))
            
            # Concurrency & Request Latency (End-to-End)
            conc = metrics.get('request_concurrency', {}).get('total', {})
            conc_mean.append(conc.get('mean', 0))
            
            reqlat = metrics.get('request_latency', {}).get('successful', {})
            # It's reported in seconds in GuideLLM, multiply by 1000 for ms
            req_lat_mean.append(reqlat.get('mean', 0) * 1000)
            
    except Exception as e:
        print(f"Warning: Could not parse letencies from {filepath}: {e}")
        
    return labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean

def parse_window_to_seconds(window: str) -> int:
    unit = window[-1]
    value = int(window[:-1])
    if unit == 'h':
        return value * 3600
    elif unit == 'm':
        return value * 60
    elif unit == 'd':
        return value * 86400
    elif unit == 's':
        return value
    return value * 3600 # Default to hours if parse fails

def main():
    parser = argparse.ArgumentParser(description="Fetch startup metrics and plot usage metrics from Prometheus.")
    parser.add_argument(
        "-n", "--namespace",
        default="asmalvan-test",
        help="The namespace to query (default: asmalvan-test)"
    )
    parser.add_argument(
        "-w", "--window",
        default="1h",
        help="The time window to look back for metrics, e.g., '30m', '1h', '2h' (default: 1h)"
    )
    parser.add_argument(
        "-o", "--output",
        default="metrics_usage.png",
        help="The output filename for the plots (default: metrics_usage.png)"
    )
    parser.add_argument(
        "-r", "--results-dir",
        default=None,
        help="Path to a GuideLLM exp-docs folder to parse results.json for succeed/failed requests."
    )
    parser.add_argument(
        "-t", "--token",
        default=None,
        help="OpenShift login token (for CI/CD environments)."
    )
    parser.add_argument(
        "-s", "--server",
        default=None,
        help="OpenShift server URL (for CI/CD environments)."
    )
    args = parser.parse_args()
    
    check_privileges(args.token, args.server)
    
    namespace = args.namespace
    window = args.window
    output_png = args.output
    results_dir = args.results_dir

    if results_dir and output_png == "metrics_usage.png":
        basename = os.path.basename(os.path.normpath(results_dir))
        output_png = f"metrics_usage_{basename}.png"

    window_seconds = parse_window_to_seconds(window)
    end_time = datetime.datetime.now().timestamp()
    start_time = end_time - window_seconds
    
    if results_dir:
        import yaml
        yaml_first = os.path.join(results_dir, "benchmark_report,_results.json_0.yaml")
        yaml_first_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_0.yaml")
        start_yaml = yaml_first if os.path.exists(yaml_first) else (yaml_first_alt if os.path.exists(yaml_first_alt) else None)
        
        yaml_last = os.path.join(results_dir, "benchmark_report,_results.json_3.yaml")
        yaml_last_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_3.yaml")
        end_yaml = yaml_last if os.path.exists(yaml_last) else (yaml_last_alt if os.path.exists(yaml_last_alt) else None)
        
        if start_yaml and end_yaml:
            try:
                with open(start_yaml, 'r') as f:
                    d_start = yaml.safe_load(f)
                    start_time = float(d_start['metrics']['time']['start']) - 60
                with open(end_yaml, 'r') as f:
                    d_end = yaml.safe_load(f)
                    end_time = float(d_end['metrics']['time']['stop']) + 60
                window_seconds = int(end_time - start_time)
                window = f"{window_seconds}s"
                print(f"\nClamping Prometheus queries exactly to benchmark duration: {datetime.datetime.fromtimestamp(start_time)} -> {datetime.datetime.fromtimestamp(end_time)}\n")
            except Exception as e:
                print(f"Warning: Failed to parse exact benchmark bounds from YAMLs, falling back to window: {e}")

    print(f"Using namespace: {namespace}")
    print(f"Using time window: {window}")
    print("-" * 50)

    print("Discovering Active Pods in Benchmark Window...")
    active_query = f'max_over_time(kube_pod_status_phase{{namespace="{namespace}",pod=~".*decode.*"}}[{window}:15s])'
    active_data = query_prometheus(active_query, eval_time=end_time)
    
    valid_pods = set()
    if active_data.get('status') == 'success' and 'result' in active_data['data']:
        for result in active_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod:
                valid_pods.add(pod)

    print("Fetching Pod creation times (Start Time)...")
    pending_query = f'max_over_time(kube_pod_start_time{{namespace="{namespace}",pod=~".*decode.*"}}[3h])'
    pending_data = query_prometheus(pending_query, eval_time=end_time)
    
    print("Fetching Container ready times (vLLM)...")
    ready_query = f'min_over_time((timestamp(kube_pod_container_status_ready{{namespace="{namespace}",pod=~".*decode.*",container="vllm"}}==1))[3h:1m])'
    ready_data = query_prometheus(ready_query, eval_time=end_time)
    
    print("Fetching Node assignments...")
    # Node assignments require historical querying because finished/failed pods drop from instantaneous queries
    node_query = f'max_over_time(kube_pod_info{{namespace="{namespace}",pod=~".*decode.*"}}[3h:1m])'
    node_data = query_prometheus(node_query, eval_time=end_time)

    pod_stats = {}

    if node_data.get('status') == 'success' and 'result' in node_data['data']:
        for result in node_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                node = result['metric'].get('node', 'Unknown')
                if node != 'Unknown':
                    pod_stats[pod]['node'] = node

    if pending_data.get('status') == 'success' and 'result' in pending_data['data']:
        for result in pending_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                val = float(result['value'][1])
                # We want minimum timestamp
                if 'pending_time' not in pod_stats[pod] or val < pod_stats[pod]['pending_time']:
                    pod_stats[pod]['pending_time'] = val

    if ready_data.get('status') == 'success' and 'result' in ready_data['data']:
        for result in ready_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                val = float(result['value'][1])
                if 'ready_time' not in pod_stats[pod] or val < pod_stats[pod]['ready_time']:
                    pod_stats[pod]['ready_time'] = val

    node_gpus = get_node_gpus()

    report_text = "\n" + "="*115 + "\n"
    report_text += f"{'Pod Name':<53} | {'Node':<20} | {'GPU':<20} | {'Startup (sec)':<15}\n"
    report_text += "="*115 + "\n"
    
    found_pods = False
    for pod, data in pod_stats.items():
        found_pods = True
        node = data.get('node', 'Unknown')
        gpu = node_gpus.get(node, "Unknown/None")
        
        pending_time = data.get('pending_time')
        ready_time = data.get('ready_time')
        
        if pending_time and ready_time:
            startup_time = round(ready_time - pending_time, 2)
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | {startup_time}s\n"
        elif ready_time:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A (Missing Pending Data)\n"
        elif pending_time:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A (Missing Ready Data)\n"
        else:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A\n"
            
    if not found_pods:
        report_text += f"No decode pods found in the {window} history for namespace: {namespace}\n"

    epp_yaml = get_epp_config(namespace)
    report_text += "\n" + "="*115 + "\n"
    report_text += "EPP Configuration (Feature Gates & Scorer Weights)\n"
    report_text += "="*115 + "\n"
    report_text += epp_yaml.strip() + "\n"

    bench_yaml = get_benchmark_config(namespace)
    report_text += "\n" + "="*115 + "\n"
    report_text += "Benchmark Load Generator Configuration\n"
    report_text += "="*115 + "\n"
    report_text += bench_yaml.strip() + "\n"

    autoscaling_config, extracted_va_cost = get_autoscaling_config(namespace)
    report_text += "\n" + "="*115 + "\n"
    report_text += "Autoscaling Configuration (HPA & VA)\n"
    report_text += "="*115 + "\n"
    report_text += autoscaling_config.strip() + "\n"

    req_x, req_succ, req_fail, req_incomp = [], [], [], []
    latency_labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean = [], [], [], [], [], [], [], []
    has_results = False
    if results_dir:
        results_file = os.path.join(results_dir, "results.json")
        if os.path.exists(results_file):
            print(f"\nProcessing GuideLLM results from {results_file}...")
            
            # 1. Parse capacity and append to text report
            capacity_report = get_true_serving_capacity(results_file)
            report_text += "\n" + "="*115 + "\n"
            report_text += capacity_report
            
            req_x, req_succ, req_fail, req_incomp = process_guidellm_results(results_file)
            latency_labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean = parse_guidellm_latencies(results_file)
            if req_x or latency_labels:
                has_results = True
            if not req_x:
                print("No requests found in results.json.")
                
            # --- AUTOSCALING SCORING CALCULATION ---
            if has_results and len(ttft_p99) > 0 and len(itl_p99) > 0:
                # Find maximum worst-case P99 latencies across all benchmarks
                max_ttft_p99 = max(ttft_p99)
                max_itl_p99 = max(itl_p99)
                
                # Fetch average replicas
                avg_repl = 1.0
                try:
                    rep_query = f'count(kube_pod_info{{namespace="{namespace}", pod=~".*decode.*"}}) by (namespace)'
                    rep_data_temp = query_prometheus_range(rep_query, start_time, end_time, step="15s", user_workload=False)
                    if rep_data_temp.get('status') == 'success' and 'result' in rep_data_temp['data'] and len(rep_data_temp['data']['result']) > 0:
                        values = rep_data_temp['data']['result'][0].get('values', [])
                        if values:
                            y_vals = [float(v[1]) for v in values]
                            if len(y_vals) > 0:
                                avg_repl = sum(y_vals) / len(y_vals)
                except Exception as e:
                    print(f"Warning: Could not fetch replica count for scoring: {e}")

                # Target SLAs
                target_ttft = 50.0 # Strict 50 ms SLA as requested
                target_itl = 50.0 # 50 ms SLA
                
                # Calculate penalties
                ttft_penalty = max_ttft_p99 / target_ttft
                itl_penalty = max_itl_p99 / target_itl
                
                # Apply the Extracted VA Cost Factor and Avg Replicas
                final_score = avg_repl * extracted_va_cost * (ttft_penalty + itl_penalty)
                
                report_text += "\n" + "="*115 + "\n"
                report_text += "Autoscaling Run Score (Lower is Better)\n"
                report_text += "="*115 + "\n"
                report_text += f"Worst-Case P99 TTFT: {max_ttft_p99:.2f} ms\n"
                report_text += f"Worst-Case P99 ITL:  {max_itl_p99:.2f} ms\n"
                report_text += f"Average Replicas:    {avg_repl:.2f}\n\n"
                report_text += f"Target SLAs: TTFT = {target_ttft}ms | ITL = {target_itl}ms\n\n"
                report_text += "Formula Engine:\n"
                report_text += "1. TTFT Penalty = (Actual P99 TTFT) / (SLA TTFT)\n"
                report_text += "2. ITL Penalty = (Actual P99 ITL) / (SLA ITL)\n"
                report_text += f"3. Cost Factor = VA Custom Resource declared Cost ({extracted_va_cost})\n"
                report_text += f"4. Avg Replicas Factor = Average running decode pods ({avg_repl:.2f})\n"
                report_text += "Autoscaling Score = Avg_Replicas × Cost_Factor × (TTFT_Penalty + ITL_Penalty)\n\n"
                report_text += f"Latency Penalty Subtotal = ({max_ttft_p99:.2f}/{target_ttft}) + ({max_itl_p99:.2f}/{target_itl}) = {(ttft_penalty + itl_penalty):.2f}\n"
                report_text += f"Resource Multiplier = {avg_repl:.2f} × {extracted_va_cost} = {(avg_repl * extracted_va_cost):.2f}\n"
                report_text += f"=> Final Score = {(avg_repl * extracted_va_cost):.2f} × {(ttft_penalty + itl_penalty):.2f} = {final_score:.2f}\n"
                
                # --- CALCULATE INTER-PHASE DELAYS ---
                report_text += "\n" + "="*115 + "\n"
                report_text += "GuideLLM Benchmark Inter-Phase Delay Analysis\n"
                report_text += "="*115 + "\n"
                import glob
                import yaml
                yaml_files = sorted(glob.glob(os.path.join(results_dir, 'benchmark_report,_results.json_*.yaml')))
                if not yaml_files:
                    yaml_files = sorted(glob.glob(os.path.join(results_dir, 'benchmark_report_v0.2,_results.json_*.yaml')))
                if yaml_files:
                    previous_stop = None
                    for idx, yaml_file in enumerate(yaml_files):
                        try:
                            with open(yaml_file, 'r') as f:
                                data = yaml.safe_load(f)
                                current_start = float(data.get('metrics', {}).get('time', {}).get('start', 0))
                                current_stop = float(data.get('metrics', {}).get('time', {}).get('stop', 0))
                                
                                file_name = os.path.basename(yaml_file)
                                
                                if previous_stop is not None and current_start > 0:
                                    delay = current_start - previous_stop
                                    report_text += f"Gap between Phase {idx-1} and {idx}: {delay:.2f} seconds\n"
                                
                                duration = current_stop - current_start
                                report_text += f"Phase {idx} ({file_name}): Start = {current_start:.2f}, Stop = {current_stop:.2f} (Duration: {duration:.1f}s)\n"
                                
                                previous_stop = current_stop
                        except Exception as e:
                            report_text += f"Warning: Could not parse {os.path.basename(yaml_file)} for delay analysis: {e}\n"
                else:
                    report_text += "Warning: No *_results.json_*.yaml files found to analyze delays.\n"
                
        else:
            print(f"\n❌ ERROR: results.json not found in {results_dir}")

    print(report_text)

    # ---- PLOTTING METRICS ----
    
    kv_query = f'vllm:kv_cache_usage_perc{{namespace="{namespace}"}}'
    kv_data = query_prometheus_range(kv_query, start_time, end_time, step="15s", user_workload=True)
    
    req_query = f'vllm:num_requests_waiting{{namespace="{namespace}"}}'
    req_data = query_prometheus_range(req_query, start_time, end_time, step="15s", user_workload=True)
    
    rep_query = f'count(kube_pod_info{{namespace="{namespace}", pod=~".*decode.*"}}) by (namespace)'
    rep_data = query_prometheus_range(rep_query, start_time, end_time, step="15s", user_workload=False)

    hpa_query = f'kube_horizontalpodautoscaler_status_desired_replicas{{namespace="{namespace}"}}'
    hpa_data = query_prometheus_range(hpa_query, start_time, end_time, step="15s", user_workload=False)
    
    wva_query = f'wva_desired_replicas{{namespace="{namespace}"}}'
    wva_data = query_prometheus_range(wva_query, start_time, end_time, step="15s", user_workload=True)

    # Query 4: EPP Flow Control Queue Size
    epp_query = f'sum(inference_extension_flow_control_queue_size{{namespace="{namespace}"}})'
    epp_data = query_prometheus_range(epp_query, start_time, end_time, step="15s", user_workload=True)
    
    has_kv = kv_data.get('status') == 'success' and 'result' in kv_data['data'] and len(kv_data['data']['result']) > 0
    has_req = req_data.get('status') == 'success' and 'result' in req_data['data'] and len(req_data['data']['result']) > 0
    has_rep = rep_data.get('status') == 'success' and 'result' in rep_data['data'] and len(rep_data['data']['result']) > 0
    has_epp = epp_data.get('status') == 'success' and 'result' in epp_data['data'] and len(epp_data['data']['result']) > 0

    if has_kv or has_req or has_rep or has_epp or has_results:
        num_subplots = 3 + (5 if has_results else 0)
        fig, axes = plt.subplots(num_subplots, 1, figsize=(12, 4 * num_subplots))
        if num_subplots == 1:
            axes = [axes]
            
        ax_kv, ax_req, ax_rep = axes[0], axes[1], axes[2]
        
        if has_kv:
            plotted_series = 0
            for result in kv_data['data']['result']:
                pod = result['metric'].get('pod', 'Unknown Pod')
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) * 100 for v in values] # Convert metric to percentage
                
                ax_kv.plot(x_times, y_vals, label=pod, linewidth=1.5)
                plotted_series += 1
                
            if plotted_series > 0:
                ax_kv.set_title(f'KV Cache Usage Percentage Over Time (Namespace: {namespace})')
                ax_kv.set_ylabel('KV Cache Usage (%)')
                ax_kv.set_ylim(0, 100) # Optional: bound Y-axis from 0 to 100%
                ax_kv.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_kv.grid(True, linestyle='--', alpha=0.7)
                ax_kv.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
        else:
            ax_kv.set_title(f'KV Cache Usage Percentage Over Time (Namespace: {namespace}) - NO DATA')
            
        if has_req:
            plotted_series = 0
            for result in req_data['data']['result']:
                pod = result['metric'].get('pod', 'Unknown Pod')
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) for v in values]
                
                ax_req.plot(x_times, y_vals, label=pod, linewidth=1.5)
                plotted_series += 1
                
            if plotted_series > 0:
                ax_req.set_title(f'Number of Requests Waiting Over Time (Namespace: {namespace})')
                ax_req.set_ylabel('Requests Waiting')
                ax_req.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_req.grid(True, linestyle='--', alpha=0.7)
                ax_req.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
        else:
            ax_req.set_title(f'Number of Requests Waiting Over Time (Namespace: {namespace}) - NO DATA')

        if has_rep:
            plotted_series = 0
            for result in rep_data['data']['result']:
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) for v in values]
                
                line_rep = ax_rep.step(x_times, y_vals, label="Actual Replicas", linewidth=2.0, color='blue', where='post')
                plotted_series += 1

            # Plot HPA Desired Replicas
            if hpa_data.get('status') == 'success' and 'result' in hpa_data['data']:
                for result in hpa_data['data']['result']:
                    values = result.get('values', [])
                    if values:
                        x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                        y_vals = [float(v[1]) for v in values]
                        ax_rep.step(x_times, y_vals, label="HPA Desired Replicas", linewidth=2.0, color='purple', linestyle='--', where='post')

            # Plot WVA Desired Replicas
            if wva_data.get('status') == 'success' and 'result' in wva_data['data']:
                for result in wva_data['data']['result']:
                    values = result.get('values', [])
                    if values:
                        x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                        y_vals = [float(v[1]) for v in values]
                        ax_rep.step(x_times, y_vals, label="WVA Desired Replicas", linewidth=2.0, color='green', linestyle=':', where='post')

            if has_epp and plotted_series > 0:
                ax_epp = ax_rep.twinx()
                for result in epp_data['data']['result']:
                    epp_values = result.get('values', [])
                    if not epp_values:
                        continue
                    
                    e_x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in epp_values]
                    e_y_vals = [float(v[1]) for v in epp_values]
                    
                    line_epp = ax_epp.fill_between(e_x_times, e_y_vals, color='orange', alpha=0.3, label="EPP Queue Size")
                    line_epp_border = ax_epp.plot(e_x_times, e_y_vals, color='darkorange', linewidth=1.5)
                    
                ax_epp.set_ylabel('EPP Flow Control Queue Size', color='darkorange')
                ax_epp.tick_params(axis='y', labelcolor='darkorange')
                
            if plotted_series > 0:
                ax_rep.set_title(f'Decode Replica Count & EPP Queue Over Time (Namespace: {namespace})')
                ax_rep.set_xlabel('Time')
                ax_rep.set_ylabel('Replica Count', color='blue')
                ax_rep.tick_params(axis='y', labelcolor='blue')
                ax_rep.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_rep.grid(True, linestyle='--', alpha=0.7)
                
                # Combine legends if EPP exists
                if has_epp:
                    lines, labels = ax_rep.get_legend_handles_labels()
                    lines2, labels2 = ax_epp.get_legend_handles_labels()
                    ax_rep.legend(lines + lines2, labels + labels2, loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
                else:
                    ax_rep.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
                
                # Ensure Y axis uses integers since replica counts are whole numbers
                ax_rep.yaxis.set_major_locator(plt.MaxNLocator(integer=True))
        else:
            ax_rep.set_title(f'Decode Replica Count Over Time (Namespace: {namespace}) - NO DATA')
            
        if has_results:
            ax_res = axes[3]
            ax_ttft = axes[4]
            ax_itl = axes[5]
            ax_tps = axes[6]
            ax_conc = axes[7]
            
            # Since req_succ, req_fail, req_incomp are lists of (count/step_seconds), we need to reconstruct the total sum
            # The easiest way is to sum the raw values and multiply by step_seconds, because y = counts / step
            # Actually, the parsing function used `bins[b]['success'] / step_seconds`. To get the exact total,
            # we should just pass the totals back from parse_guidellm_results, or we can approximate by multiplying back.
            # However, for 100% accuracy, let's reverse the math: total_count = sum( RPS * step_seconds ).
            # Assuming step_seconds is 15.
            total_succ = int(sum(req_succ) * 15)
            total_fail = int(sum(req_fail) * 15)
            total_incomp = int(sum(req_incomp) * 15)
            
            ax_res.plot(req_x, req_succ, label=f"Successful RPS (Total: {total_succ})", linewidth=2.0, color='green')
            
            if total_fail > 0:
                ax_res.plot(req_x, req_fail, label=f"Failed RPS (Total: {total_fail})", linewidth=2.0, color='red', linestyle='--')
            if total_incomp > 0:
                ax_res.plot(req_x, req_incomp, label=f"Incomplete RPS (Total: {total_incomp})", linewidth=2.0, color='orange', linestyle='-.')
            
            ax_res.set_title(f'GuideLLM Requests (Succeeded vs Failed vs Incomplete) Over Time')
            ax_res.set_xlabel('Time')
            ax_res.set_ylabel('Requests / Second (RPS)')
            ax_res.grid(True, linestyle='--', alpha=0.7)
            ax_res.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            
            # TTFT and ITL plots
            import numpy as np
            x_pos = np.arange(len(latency_labels))
            width = 0.35
            
            # TTFT Bar Chart
            ax_ttft.bar(x_pos - width/2, ttft_mean, width, label='Mean TTFT', color='skyblue')
            ax_ttft.bar(x_pos + width/2, ttft_p99, width, label='P99 TTFT', color='salmon')
            ax_ttft.set_title('Time To First Token (TTFT) per Run')
            ax_ttft.set_ylabel('TTFT (ms, log scale)')
            ax_ttft.set_yscale('log')
            ax_ttft.set_xticks(x_pos)
            ax_ttft.set_xticklabels(latency_labels)
            ax_ttft.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_ttft.grid(True, axis='y', linestyle='--', alpha=0.7)

            # ITL Bar Chart
            ax_itl.bar(x_pos - width/2, itl_mean, width, label='Mean ITL', color='lightgreen')
            ax_itl.bar(x_pos + width/2, itl_p99, width, label='P99 ITL', color='orchid')
            ax_itl.set_title('Inter-Token Latency (ITL) per Run')
            ax_itl.set_ylabel('ITL (ms, log scale)')
            ax_itl.set_yscale('log')
            ax_itl.set_xticks(x_pos)
            ax_itl.set_xticklabels(latency_labels)
            ax_itl.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_itl.grid(True, axis='y', linestyle='--', alpha=0.7)
            
            # Token Throughput Bar Chart
            ax_tps.bar(x_pos, tps_mean, width*1.5, label='Mean Tokens/s', color='gold')
            ax_tps.set_title('Overall Token Throughput per Run')
            ax_tps.set_ylabel('Tokens / Second')
            ax_tps.set_xticks(x_pos)
            ax_tps.set_xticklabels(latency_labels)
            
            # Annotate bars with standard value formatting
            for i, v in enumerate(tps_mean):
                ax_tps.text(i, v + (max(tps_mean)*0.01), f"{int(v)}", ha='center', va='bottom', fontsize=9)
                
            ax_tps.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_tps.grid(True, axis='y', linestyle='--', alpha=0.7)
            
            # Concurrency vs Request Latency Dual Axis Plot
            # Bar chart for Total Concurrency (Left Axis)
            bar1 = ax_conc.bar(x_pos, conc_mean, width*1.5, label='Mean Concurrency', color='green', alpha=0.7)
            ax_conc.set_title('Request Concurrency vs End-to-End Latency Profile')
            ax_conc.set_ylabel('Total In-flight Requests', color='darkgreen')
            ax_conc.tick_params(axis='y', labelcolor='darkgreen')
            ax_conc.set_xticks(x_pos)
            ax_conc.set_xticklabels(latency_labels)
            
            # Line chart for Request Latency (Right Axis)
            ax_conc_tw = ax_conc.twinx()
            line1 = ax_conc_tw.plot(x_pos, req_lat_mean, color='red', marker='o', linewidth=2.5, label='Mean Request Latency')
            ax_conc_tw.set_ylabel('Total Request Latency (ms)', color='red')
            ax_conc_tw.tick_params(axis='y', labelcolor='red')
            
            # Combine legends
            bars_lines = [bar1, line1[0]]
            labels = [l.get_label() for l in bars_lines]
            ax_conc.legend(bars_lines, labels, loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_conc.grid(True, axis='y', linestyle='--', alpha=0.4)
        # Rotate dates manually instead of autofmt_xdate which hides top axis labels
        for ax in axes:
            plt.setp(ax.get_xticklabels(), rotation=30, ha='right')

        plt.tight_layout()
        plt.savefig(output_png, bbox_inches="tight")
        
        pdf_filename = output_png.replace(".png", ".pdf")
        if not output_png.endswith(".png"):
            pdf_filename = output_png + ".pdf"
            
        with PdfPages(pdf_filename) as pdf:
            # --- Text Pagination Logic ---
            lines = report_text.split('\n')
            lines_per_page = 60 # Adjust based on font size and figure size
            
            for i in range(0, len(lines), lines_per_page):
                page_lines = lines[i:i+lines_per_page]
                page_text = '\n'.join(page_lines)
                
                fig_text = plt.figure(figsize=(10, 11))
                fig_text.clf()
                fig_text.text(0.05, 0.95, page_text, family='monospace', size=8, va='top', ha='left')
                pdf.savefig(fig_text)
                plt.close(fig_text)
            
            # --- Append Metrics Plots ---
            pdf.savefig(fig, bbox_inches="tight")
            
        print(f"Metrics plot successfully saved to {output_png}")
        print(f"Full PDF report successfully saved to {pdf_filename}")
    else:
        print(f"No metric data found for namespace: {namespace} in the last {window}.")


if __name__ == "__main__":
    main()
