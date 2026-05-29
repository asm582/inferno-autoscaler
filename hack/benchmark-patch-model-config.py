#!/usr/bin/env python3
"""Patch model.name / model.huggingfaceId in rendered workspace config.yaml files.

llmdbenchmark bakes the scenario's default model into the rendered config.yaml
during standup.  When the user overrides the model via -m / BENCHMARK_MODEL_ID,
the rendered file keeps the scenario default, causing the run summary to display
the wrong model name.  This script rewrites the relevant fields after standup.

Usage: benchmark-patch-model-config.py <workspace_dir> <model_id>
"""

import pathlib
import sys

import yaml


def main():
    if len(sys.argv) != 3:
        print(f"Usage: {sys.argv[0]} <workspace_dir> <model_id>", file=sys.stderr)
        sys.exit(1)

    workspace = pathlib.Path(sys.argv[1])
    model_id = sys.argv[2]

    patched = 0
    for cfg_path in (workspace / "plan").glob("*/config.yaml"):
        text = cfg_path.read_text(encoding="utf-8")
        cfg = yaml.safe_load(text) or {}
        model = cfg.get("model")
        if not isinstance(model, dict):
            continue
        model["name"] = model_id
        model["huggingfaceId"] = model_id
        cfg_path.write_text(yaml.dump(cfg, default_flow_style=False), encoding="utf-8")
        patched += 1

    print(f"Patched model ID to '{model_id}' in {patched} config.yaml file(s).")


if __name__ == "__main__":
    main()
