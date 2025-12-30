
import requests
import time
import json
import sys

# Configuration
# Configuration
# Configuration
url = "http://localhost:8083/v1/completions"
model = "Qwen/Qwen2.5-0.5B-Instruct"
headers = {
    "Content-Type": "application/json",
    "x-prediction-based-scheduling": "true",
    "x-slo-ttft-ms": "200",
    "x-slo-tpot-ms": "50",
    "x-inference-schedule": "1"
}
data = {
    "model": model,
    "prompt": "Hello, how are you? " * 50,
    "max_tokens": 200,
    "stream": True,
    "stream_options": {"include_usage": True}
}

print(f"Starting load generation to {url}...")

success_count = 0
fail_count = 0

from concurrent.futures import ThreadPoolExecutor

def send_request():
    try:
        response = requests.post(url, headers=headers, json=data, timeout=5)
        return response.status_code
    except Exception as e:
        return str(e)

with ThreadPoolExecutor(max_workers=100) as executor:
    while True:
        futures = [executor.submit(send_request) for _ in range(100)]
        for f in futures:
            res = f.result()
            if res == 200:
                success_count += 1
                if success_count % 50 == 0:
                    print(f"Sent {success_count} requests.")
            else:
                 print(f"Failed: {res}")
        time.sleep(0.01)
