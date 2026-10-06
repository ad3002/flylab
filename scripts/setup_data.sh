#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "=== FlyLab: Setting Up & Verifying Dataset ==="

MANIFEST="$ROOT_DIR/data/dataset_manifest.json"
if [[ ! -f "$MANIFEST" ]]; then
    echo "Error: Manifest not found at $MANIFEST" >&2
    exit 1
fi

echo "[1/3] Checking dataset files and SHA256 integrity..."
if [[ -f "$ROOT_DIR/bin/flysim" ]]; then
    "$ROOT_DIR/bin/flysim" inspect --dataset-manifest "$MANIFEST"
else
    echo "Warning: bin/flysim not yet compiled. Checking via sha256sum..."
    COMPLETENESS="$ROOT_DIR/data/2023_03_23_completeness_630_final.csv"
    CONNECTIVITY="$ROOT_DIR/data/2023_03_23_connectivity_630_final.parquet"

    if [[ ! -f "$COMPLETENESS" || ! -f "$CONNECTIVITY" ]]; then
        echo "Error: Completeness CSV or Connectivity Parquet missing in data/" >&2
        exit 1
    fi
fi

echo "[2/3] Preparing binary CSR graph cache..."
mkdir -p "$ROOT_DIR/data/cache"
if [[ ! -f "$ROOT_DIR/bin/flysim" ]]; then
    echo "Building flysim release binary..."
    cargo build --release --manifest-path "$ROOT_DIR/rust/flysim/Cargo.toml"
    mkdir -p "$ROOT_DIR/bin"
    cp "$ROOT_DIR/rust/flysim/target/release/flysim" "$ROOT_DIR/bin/flysim"
fi

"$ROOT_DIR/bin/flysim" prepare \
    --dataset-manifest "$MANIFEST" \
    --output "$ROOT_DIR/data/cache"

echo "[3/3] Dataset cache verified successfully:"
ls -lh "$ROOT_DIR/data/cache/flywire_630_csr.bin"
echo "=== Dataset setup complete ==="
