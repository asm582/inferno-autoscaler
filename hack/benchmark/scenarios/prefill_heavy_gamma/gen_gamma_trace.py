#!/usr/bin/env python3
"""Generate a Gamma-distributed request-arrival trace for guidellm replay.

Inter-arrival times are drawn from Gamma(shape=k, scale=1/(k*rate)):
  k < 1  → spikier / more bursty than Poisson
  k = 1  → equivalent to Poisson (exponential inter-arrivals)
  k > 1  → smoother, sub-Poisson

Usage:
  python gen_gamma_trace.py [--rate RPS] [--duration SECONDS] \
      [--shape K] [--prompt-tokens N] [--output-tokens N] \
      [--seed SEED] [--out PATH]
"""

import argparse
import json

import numpy as np


def parse_args():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--rate", type=float, default=20.0, help="Target requests per second (default: 20)")
    p.add_argument("--duration", type=float, default=600.0, help="Trace duration in seconds (default: 600)")
    p.add_argument("--shape", type=float, default=1.0, help="Gamma shape k: <1 bursty, 1=Poisson, >1 smooth (default: 1.0)")
    p.add_argument("--prompt-tokens", type=int, default=4000, help="Prompt token count per request (default: 4000)")
    p.add_argument("--output-tokens", type=int, default=1000, help="Output token count per request (default: 1000)")
    p.add_argument("--seed", type=int, default=42, help="Random seed (default: 42)")
    p.add_argument("--out", default="test/benchmark/scenarios/traces/prefill_heavy_gamma_trace.jsonl", help="Output JSONL path (default: test/benchmark/scenarios/traces/prefill_heavy_gamma_trace.jsonl)")
    return p.parse_args()


def main():
    args = parse_args()

    rng = np.random.default_rng(args.seed)

    # mean inter-arrival = 1/rate; Gamma(k, scale=1/(k*rate)) preserves that mean
    scale = 1.0 / (args.shape * args.rate)
    # generate enough requests to cover the full duration with margin
    n = int(args.rate * args.duration * 2)
    inter_arrivals = rng.gamma(shape=args.shape, scale=scale, size=n)
    timestamps = np.cumsum(inter_arrivals)

    # keep only requests that fall within the requested duration
    timestamps = timestamps[timestamps <= args.duration]

    with open(args.out, "w") as f:
        for ts in timestamps:
            f.write(json.dumps({
                "timestamp": round(float(ts), 6),
                "input_length": args.prompt_tokens,
                "output_length": args.output_tokens,
            }) + "\n")

    print(f"Wrote {len(timestamps)} requests over {args.duration}s "
          f"(actual rate: {len(timestamps)/args.duration:.2f} RPS) → {args.out}")


if __name__ == "__main__":
    main()
