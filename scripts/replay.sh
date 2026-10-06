#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

TARGET="${1:-${PLAN:-artifacts/demo_run}}"

echo "=== FlyLab: Replay Runner ==="
echo "Target: $TARGET"

TEMP_DIR=$(mktemp -d "/tmp/flylab_replay_XXXXXX")
trap 'rm -rf "$TEMP_DIR"' EXIT

ARCHIVE_DIR=""

if [[ -f "$TARGET" && "$TARGET" == *.zip ]]; then
    echo "Extracting ZIP archive $TARGET..."
    unzip -q "$TARGET" -d "$TEMP_DIR/extracted"
    ARCHIVE_DIR="$TEMP_DIR/extracted"
elif [[ -d "$TARGET" ]]; then
    ARCHIVE_DIR="$TARGET"
elif [[ -f "$TARGET" && "$TARGET" == *.json ]]; then
    echo "Direct resolved plan provided: $TARGET"
    mkdir -p "$TEMP_DIR/single_run"
    "$ROOT_DIR/bin/flysim" run \
        --resolved-plan "$TARGET" \
        --output "$TEMP_DIR/single_run" \
        --cache-dir "$ROOT_DIR/data/cache" \
        --manifest "$ROOT_DIR/data/dataset_manifest.json"
    cat "$TEMP_DIR/single_run/summary.json"
    exit 0
else
    echo "Error: Target must be a zip file, export directory, or resolved plan JSON file." >&2
    exit 1
fi

PLAN_FILE="$ARCHIVE_DIR/resolved_plan.json"
EVENTS_FILE="$ARCHIVE_DIR/input_events.parquet"
MANIFEST_FILE="$ARCHIVE_DIR/manifest.json"

if [[ ! -f "$PLAN_FILE" ]]; then
    echo "Error: resolved_plan.json not found in $ARCHIVE_DIR" >&2
    exit 1
fi

if [[ ! -f "$EVENTS_FILE" ]]; then
    echo "Notice: input_events.parquet missing. Running standard simulation..."
    "$ROOT_DIR/bin/flysim" run \
        --resolved-plan "$PLAN_FILE" \
        --output "$TEMP_DIR/output" \
        --cache-dir "$ROOT_DIR/data/cache" \
        --manifest "${MANIFEST_FILE:-$ROOT_DIR/data/dataset_manifest.json}"
    cat "$TEMP_DIR/output/summary.json"
    exit 0
fi

OUTPUT_DIR="$TEMP_DIR/replayed_output"
mkdir -p "$OUTPUT_DIR"

echo "Executing offline replay using Rust core (zero Python, zero web overhead)..."
"$ROOT_DIR/bin/flysim" replay \
    --resolved-plan "$PLAN_FILE" \
    --input-events "$EVENTS_FILE" \
    --manifest "${MANIFEST_FILE:-$ROOT_DIR/data/dataset_manifest.json}" \
    --output "$OUTPUT_DIR" \
    --cache-dir "$ROOT_DIR/data/cache"

echo ""
echo "=== Replay Completed Successfully ==="
cat "$OUTPUT_DIR/summary.json"
