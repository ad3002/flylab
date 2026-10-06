#!/usr/bin/env bash
set -euo pipefail
flysim replay --resolved-plan resolved_plan.json --input-events input_events.parquet --manifest manifest.json --output replayed_output
