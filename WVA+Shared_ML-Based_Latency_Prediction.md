# WVA \+ ML-Based Latency Prediction

## [kaushik mitra](mailto:kaushikmitra.umd@gmail.com)

## Summary

The Workload Variant Autoscaler (WVA) currently relies on first-order analytical approximations (e.g., linear regression for ITL) to model system performance. While computationally inexpensive, these models rely on simplifying assumptions—such as the independence of latency from sequence length—that degrade in accuracy under complex, high-variance workloads.

This note details the integration of the [**Predicted-Latency** architecture](https://gateway-api-inference-extension.sigs.k8s.io/guides/latency-based-predictor/)—originally built for the scheduler—directly into the WVA. By consuming the *same* high-fidelity XGBoost predictions used for routing, the WVA transitions from reactive, utilization-based scaling to predictive, SLO-aware scaling. This ensures scaling decisions are mathematically consistent with routing decisions, preventing the "thrashing" that occurs when scheduler and autoscaler use different definitions of "saturation."

---

## 1\. Limitations of the Analytical Baseline

The current WVA performance profile assumes a linear relationship for Inter-Token Latency (ITL):

ITL \= alpha \+ beta \* KV\_Cache\_Perc

While effective for initial sizing, this model exhibits brittleness in few key areas:

1. **Linear Approximation**: The formula ignores the non linear effect kv cache size has on ITL KV Cache approaches 100%  
2. **Prefill/Decode Interference**: It struggles to account for "chunked prefill" interference, where new requests entering the batch momentarily spike the latency for existing requests.  
3. **TTFT Modeling**: To estimate TTFT we need to take into account not just load (KV Cache %, running req etc.) but also input len and the prefix cache hit.

## 2\. The Shared Model Architecture

Instead of building a separate analytical model for the WVA, we can utilize the existing **Latency Predictor** sidecar deployed for the Endpoint Picker (EPP). This architecture effectively treats the ML model as a shared "Oracle" for the entire inference control plane.

The predictor uses **XGBoost** to predict p90 TTFT and p90 ITL based on a rich feature set:

* **KV Cache Usage %**  
* **Input Length**  
* **Queue Depth**  
* **Active Request Count**  
* **Prefix Cache Match %**  
* **Hardware and deployment characteristics ([variant](https://docs.google.com/document/d/18BjoHeZO0gC3coGJSGQ7080HxrAft6c6SsSxalBOumI/edit?tab=t.0#heading=h.yzyab39lx08n) as a label)**

## 3\. Integrating Predictions into WVA Logic

The WVA determines the optimal number of replicas required to satisfy SLOs given the current load by querying the ML predictor with a combination of *real* infrastructure state and *estimated* request shapes.

### A. Direct ITL Prediction (Decode Phase)

For ITL, the WVA consumes the model's predictions using aggregate pod metrics.

* **Input**: The WVA feeds current pod state metrics (e.g., *Current Batch Size*, *KV Cache Usage*) into the model.  
* **Prediction**: The model returns the expected **p90 ITL**.  
* **Action**: If the predicted ITL approaches the SLO threshold, the WVA signals saturation. Unlike linear models which miss "knees" in performance, the XGBoost model captures non-linear degradation at high KV-cache utilization, triggering scale-up *before* latency spikes occur.

### B. TTFT Prediction (Prefill Phase)

TTFT prediction is complex because it depends heavily on request characteristics extrinsic to the pod's state: **Input Length** and **Prefix Cache Match %**.

* **The Challenge**: The WVA needs to predict TTFT for *future* requests (capacity planning) but cannot know the exact prompt length or prefix reuse of requests that haven't arrived yet.  
* **The Solution (Online Estimation)**: The WVA tracks online histograms (or digests) for two key metrics based on recent traffic history:  
  1. **Input Token Length**: Estimating the size of the prompt.  
  2. **Prefix Cache Match Rate**: Estimating how much of the prompt will be a "cache hit."  
* **Prediction Workflow**:  
  1. WVA samples the **p90 Input Length** and **p50 Prefix Match %** from its recent history trackers.  
  2. It combines these estimated features with the *current pod state* (Queue Depth, Active Requests).  
  3. It queries the XGBoost model for the predicted **TTFT**.




