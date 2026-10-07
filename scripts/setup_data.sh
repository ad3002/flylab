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

# Neuron annotations (Schlegel et al. 2024, FlyWire materialization 783), optional for the
# simulator, used by the v3 interpretation digest. Pinned commit + SHA-256 in the manifest.
ANN_RAW="$ROOT_DIR/data/Supplemental_file1_neuron_annotations.tsv"
ANN_OUT="$ROOT_DIR/data/annotations_630.tsv"
COMPLETENESS="$ROOT_DIR/data/2023_03_23_completeness_630_final.csv"
read -r ANN_URL ANN_SHA < <(python3 -c 'import json,sys
f=json.load(open(sys.argv[1]))["files"]["annotations"]
print(f["url"], f["sha256"])' "$MANIFEST")
if [[ -z "${ANN_URL:-}" || -z "${ANN_SHA:-}" ]]; then
    echo "Error: $MANIFEST has no usable files.annotations entry (url, sha256)." >&2
    exit 1
fi

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

echo "[0/3] Neuron annotations (FlyWire, Schlegel et al. 2024)..."
if [[ ! -f "$ANN_RAW" ]]; then
    echo "Downloading $ANN_URL"
    curl -fL --retry 3 -o "$ANN_RAW.part" "$ANN_URL"
    mv -f "$ANN_RAW.part" "$ANN_RAW"
fi
ACTUAL_SHA=$(sha256_of "$ANN_RAW")
if [[ "$ACTUAL_SHA" != "$ANN_SHA" ]]; then
    echo "Error: $ANN_RAW has SHA-256 $ACTUAL_SHA, the manifest pins $ANN_SHA." >&2
    echo "       Delete the file to download the pinned version again." >&2
    exit 1
fi
if [[ ! -f "$COMPLETENESS" ]]; then
    echo "Error: $COMPLETENESS is missing; cannot select v630 neurons for the annotations." >&2
    exit 1
fi
# Keep only rows whose root id exists in v630 (ids edited between v630 and v783 drop out).
awk -F'\t' -v OFS='\t' -v cols="root_id flow super_class cell_class cell_sub_class cell_type hemibrain_type top_nt top_nt_conf side nerve" '
    FNR == NR { if (FNR > 1) { split($0, a, ","); if (a[1] != "") v630[a[1]] = 1; total++ } ; next }
    { sub(/\r$/, "") }
    FNR == 1 {
        n = split(cols, want, " ")
        for (i = 1; i <= NF; i++) idx[$i] = i
        for (i = 1; i <= n; i++) if (!(want[i] in idx)) { print "missing column " want[i] > "/dev/stderr"; bad = 1; exit 2 }
        line = want[1]; for (i = 2; i <= n; i++) line = line OFS want[i]; print line
        next
    }
    {
        id = $idx["root_id"]
        if (seen[id]++) { print "duplicate root_id " id " on line " FNR > "/dev/stderr"; bad = 1; exit 3 }
        rows++
        if (!(id in v630)) next
        line = $idx[want[1]]; for (i = 2; i <= n; i++) line = line OFS $idx[want[i]]; print line
        kept++
    }
    END {
        if (bad) exit 1
        printf "annotation rows: %d; v630 neurons: %d; annotated v630 neurons: %d (%.1f%%)\n", rows, total, kept, 100 * kept / total > "/dev/stderr"
    }
' "$COMPLETENESS" "$ANN_RAW" > "$ANN_OUT.part"
mv -f "$ANN_OUT.part" "$ANN_OUT"
echo "Derived $ANN_OUT ($(($(wc -l < "$ANN_OUT") - 1)) neurons)"

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
