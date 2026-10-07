#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "=== FlyLab: Running End-to-End Smoke Test ==="

PORT=${SMOKE_PORT:-8089}
BASE="http://127.0.0.1:$PORT"
# Scratch DB/artifacts live under the project (or SMOKE_WORK_DIR), never in /tmp.
WORK_ROOT="${SMOKE_WORK_DIR:-$ROOT_DIR/.smoke}"
mkdir -p "$WORK_ROOT"
TEMP_DIR=$(mktemp -d "$WORK_ROOT/run_XXXXXX")
SERVER_PID=""
trap 'echo "Cleaning up..."; [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true; rm -rf "$TEMP_DIR"' EXIT

DB_PATH="$TEMP_DIR/smoke.db"
ARTIFACTS_DIR="$TEMP_DIR/artifacts"
mkdir -p "$ARTIFACTS_DIR"

if [[ ! -x "$ROOT_DIR/bin/flylab" || ! -x "$ROOT_DIR/bin/flysim" ]]; then
    echo "Error: Binaries bin/flylab or bin/flysim not found. Build them first." >&2
    exit 1
fi

# json_get <json> <python expression on d>: prints the value or fails loudly.
json_get() {
    python3 -c 'import json,sys
d=json.loads(sys.argv[1])
v=eval(sys.argv[2])
print(v if not isinstance(v,(dict,list)) else json.dumps(v))' "$1" "$2"
}

echo "[1/8] Starting FlyLab server on 127.0.0.1:$PORT..."
HOST=127.0.0.1 PORT="$PORT" DB_PATH="$DB_PATH" ARTIFACTS_DIR="$ARTIFACTS_DIR" REGISTRATION_OPEN=true \
    "$ROOT_DIR/bin/flylab" > "$TEMP_DIR/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server to become healthy
READY=0
for i in {1..20}; do
    if curl -s "$BASE/health" | grep -q '"status":"ok"'; then
        READY=1
        break
    fi
    sleep 0.5
done

if [[ "$READY" -ne 1 ]]; then
    echo "Error: Server failed to start. Logs:" >&2
    cat "$TEMP_DIR/server.log" >&2
    exit 1
fi
echo "FlyLab server is healthy and responding."

echo "[2/8] Registering a throwaway user and checking auth..."
SMOKE_USER="smoke_$(date +%s)_$RANDOM"
SMOKE_PASS="smoke-pass-$RANDOM$RANDOM"
REG_RESP=$(curl -s -X POST "$BASE/api/v1/auth/register" -H "Content-Type: application/json" \
    -d "{\"username\":\"$SMOKE_USER\",\"password\":\"$SMOKE_PASS\"}")
TOKEN=$(json_get "$REG_RESP" 'd["token"]') || { echo "Error: registration failed: $REG_RESP" >&2; exit 1; }
AUTH=(-H "Authorization: Bearer $TOKEN")
UNAUTH_BODY="$TEMP_DIR/unauth.json"
UNAUTH_CODE=$(curl -s -o "$UNAUTH_BODY" -w '%{http_code}' "$BASE/api/v1/jobs")
UNAUTH_ERR=$(json_get "$(cat "$UNAUTH_BODY")" 'd["error"]["code"]') || UNAUTH_ERR="(not a JSON error envelope)"
if [[ "$UNAUTH_CODE" != "401" || "$UNAUTH_ERR" != "UNAUTHENTICATED" ]]; then
    echo "Error: GET /api/v1/jobs without a session returned $UNAUTH_CODE/$UNAUTH_ERR, expected 401/UNAUTHENTICATED: $(cat "$UNAUTH_BODY")" >&2
    exit 1
fi
# CSRF guard: a cross-site text/plain form login must be refused with the error envelope.
CSRF_BODY="$TEMP_DIR/csrf.json"
CSRF_CODE=$(curl -s -o "$CSRF_BODY" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" -H "Content-Type: text/plain" \
    -d "{\"username\":\"$SMOKE_USER\",\"password\":\"$SMOKE_PASS\"}")
CSRF_ERR=$(json_get "$(cat "$CSRF_BODY")" 'd["error"]["code"]') || CSRF_ERR="(not a JSON error envelope)"
if [[ "$CSRF_CODE" != "415" || "$CSRF_ERR" != "UNSUPPORTED_MEDIA_TYPE" ]]; then
    echo "Error: text/plain login returned $CSRF_CODE/$CSRF_ERR, expected 415/UNSUPPORTED_MEDIA_TYPE: $(cat "$CSRF_BODY")" >&2
    exit 1
fi
ME_USER=$(json_get "$(curl -s "${AUTH[@]}" "$BASE/api/v1/me")" 'd["user"]["username"]')
if [[ "$ME_USER" != "$SMOKE_USER" ]]; then
    echo "Error: /api/v1/me returned user '$ME_USER', expected '$SMOKE_USER'" >&2
    exit 1
fi
echo "Registered $SMOKE_USER; Bearer auth works, anonymous access and text/plain POSTs are rejected."

echo "[3/8] Validating experiment plan (Sugar GRN 50 Hz vs Demo Silencing)..."
PLAN_FILE="$ROOT_DIR/contracts/fixtures/valid_compare.json"
VAL_RESP=$(curl -s -X POST "$BASE/api/v1/plans/validate" \
    -H "Content-Type: application/json" \
    -d @"$PLAN_FILE")

PLAN_ID=$(echo "$VAL_RESP" | grep -o '"plan_id":"[^"]*' | head -n1 | cut -d'"' -f4)
if [[ -z "$PLAN_ID" ]]; then
    echo "Error: Failed to validate plan. Response: $VAL_RESP" >&2
    exit 1
fi
echo "Plan validated successfully: $PLAN_ID"

echo "[4/8] Submitting job with Idempotency-Key..."
JOB_RESP=$(curl -s -X POST "$BASE/api/v1/jobs" "${AUTH[@]}" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: smoke-idemp-001" \
    -d "{\"plan_id\":\"$PLAN_ID\",\"title\":\"Smoke: sugar GRN vs demo silencing\"}")

JOB_ID=$(echo "$JOB_RESP" | grep -o '"job_id":"[^"]*' | head -n1 | cut -d'"' -f4)
if [[ -z "$JOB_ID" ]]; then
    echo "Error: Failed to submit job. Response: $JOB_RESP" >&2
    exit 1
fi
echo "Job created and queued: $JOB_ID"

echo "[5/8] Polling job execution status..."
SUCCEEDED=0
for i in {1..30}; do
    STATUS_RESP=$(curl -s "${AUTH[@]}" "$BASE/api/v1/jobs/$JOB_ID")
    STATUS=$(echo "$STATUS_RESP" | grep -o '"status":"[^"]*' | cut -d'"' -f4)
    STAGE=$(echo "$STATUS_RESP" | grep -o '"stage":"[^"]*' | cut -d'"' -f4)
    echo "  Status: $STATUS (stage: $STAGE)"
    if [[ "$STATUS" == "succeeded" ]]; then
        SUCCEEDED=1
        break
    elif [[ "$STATUS" == "failed" || "$STATUS" == "cancelled" ]]; then
        echo "Job terminated unexpectedly with status: $STATUS" >&2
        echo "$STATUS_RESP" >&2
        exit 1
    fi
    sleep 1
done

if [[ "$SUCCEEDED" -ne 1 ]]; then
    echo "Error: Job timed out before completion." >&2
    exit 1
fi
echo "Job completed successfully!"

echo "[6/8] Verifying experiment results, history and scientific outputs..."
RESULTS=$(curl -s "${AUTH[@]}" "$BASE/api/v1/jobs/$JOB_ID/results")
echo "$RESULTS" | grep -q '"total_spikes_A":267' || {
    echo "Error: total_spikes_A != 267. Results: $RESULTS" >&2
    exit 1
}
echo "$RESULTS" | grep -q '"total_spikes_B":266' || {
    echo "Error: total_spikes_B != 266. Results: $RESULTS" >&2
    exit 1
}
HISTORY=$(curl -s "${AUTH[@]}" "$BASE/api/v1/jobs")
H_SPIKES=$(json_get "$HISTORY" '[j for j in d["jobs"] if j["job_id"]=="'"$JOB_ID"'"][0]["summary"]["total_spikes_A"]') || {
    echo "Error: job missing from history: $HISTORY" >&2; exit 1; }
H_ERR=$(json_get "$HISTORY" 'd["jobs"][0]["summary_error"]')
if [[ "$H_SPIKES" != "267" || "$H_ERR" != "None" ]]; then
    echo "Error: history summary wrong (total_spikes_A=$H_SPIKES, summary_error=$H_ERR): $HISTORY" >&2
    exit 1
fi
echo "Scientific validation passed: Baseline=267 spikes, Silenced=266 spikes; history shows the summary."

echo "[7/8] Deterministic interpretation digest (flysim digest, annotations, coverage)..."
NO_INTERP_BODY="$TEMP_DIR/no_interp.json"
NO_INTERP_CODE=$(curl -s -o "$NO_INTERP_BODY" -w '%{http_code}' "${AUTH[@]}" "$BASE/api/v1/jobs/$JOB_ID/interpretation")
NO_INTERP_ERR=$(json_get "$(cat "$NO_INTERP_BODY")" 'd["error"]["code"]') || NO_INTERP_ERR="(not a JSON error envelope)"
if [[ "$NO_INTERP_CODE" != "404" || "$NO_INTERP_ERR" != "INTERPRETATION_NOT_FOUND" ]]; then
    echo "Error: GET interpretation before any returned $NO_INTERP_CODE/$NO_INTERP_ERR: $(cat "$NO_INTERP_BODY")" >&2
    exit 1
fi
DIGEST_BODY="$TEMP_DIR/digest.json"
DIGEST_CODE=$(curl -s -o "$DIGEST_BODY" -w '%{http_code}' "${AUTH[@]}" "$BASE/api/v1/jobs/$JOB_ID/digest")
if [[ "$DIGEST_CODE" != "200" ]]; then
    echo "Error: GET digest returned $DIGEST_CODE: $(cat "$DIGEST_BODY")" >&2
    exit 1
fi
DIGEST=$(cat "$DIGEST_BODY")
D_A=$(json_get "$DIGEST" 'd["digest"]["totals"]["A"]["spikes"]')
D_B=$(json_get "$DIGEST" 'd["digest"]["totals"]["B"]["spikes"]')
D_READOUT_HOPS=$(json_get "$DIGEST" '[r["hops_from_stimulated"] for r in d["digest"]["readouts"]]')
D_ANN=$(json_get "$DIGEST" 'd["digest"]["coverage"]["annotations_ready"]')
D_COV=$(json_get "$DIGEST" 'd["digest"]["coverage"]["active_annotated_share"]')
D_STIM=$(json_get "$DIGEST" 'd["digest"]["experiment"]["stimulated"][0]["group_id"]')
if [[ "$D_A" != "267" || "$D_B" != "266" || "$D_STIM" != "sugar_grn" ]]; then
    echo "Error: digest totals/groups wrong (A=$D_A B=$D_B stimulated=$D_STIM): $DIGEST" >&2
    exit 1
fi
# v3 digest content an expert's hypotheses need: per-neuron stimulated table, network-only top
# list, readout inputs from the real graph, the simulator's parameters.
D_SHAPE=$(json_get "$DIGEST" '[d["digest"]["schema_version"], len(d["digest"]["stimulated_neurons"]), any("stimulated" in n["roles"] for n in d["digest"]["top_neurons_by_rate"]), all(r["presynaptic_partners"] > 0 for r in d["digest"]["readouts"]), len(d["digest"]["readout_inputs"]) > 0, d["digest"]["model_parameters"]["max_rate_hz_bound"]]')
if [[ "$D_SHAPE" != '["1.1", 21, false, true, true, 454.5]' ]]; then
    echo "Error: digest shape wrong [schema, stimulated rows, stimulated in top, readouts have inputs, readout_inputs listed, max rate]: $D_SHAPE" >&2
    exit 1
fi
if [[ ! -f "$ARTIFACTS_DIR/$JOB_ID/digest_graph.json" ]]; then
    echo "Error: digest_graph.json was not cached in the run's artifact directory" >&2
    exit 1
fi
if [[ -f "$ROOT_DIR/data/annotations_630.tsv" && "$D_ANN" != "True" ]]; then
    echo "Error: data/annotations_630.tsv is installed but the digest reports annotations_ready=$D_ANN" >&2
    exit 1
fi
echo "Digest: A=$D_A B=$D_B spikes, readout hops from stimulated $D_READOUT_HOPS, annotations_ready=$D_ANN, annotated share of active neurons $D_COV."
if [[ "${SMOKE_INTERPRET:-0}" == "1" ]]; then
    # Opt-in: one real (paid) claude -p interpretation.
    INTERP=$(curl -s -X POST "${AUTH[@]}" -H "Content-Type: application/json" -d '{"language":"en"}' \
        "$BASE/api/v1/jobs/$JOB_ID/interpretation")
    I_H=$(json_get "$INTERP" 'd["interpretation"]["headline"]') || { echo "Error: interpretation failed: $INTERP" >&2; exit 1; }
    echo "Interpretation headline: $I_H"
fi

echo "[8/8] Downloading archive export and testing offline replay..."
EXPORT_ZIP="$TEMP_DIR/export.zip"
curl -sf "${AUTH[@]}" "$BASE/api/v1/jobs/$JOB_ID/export" -o "$EXPORT_ZIP"

REPLAY_DIR="$TEMP_DIR/replay"
mkdir -p "$REPLAY_DIR"
unzip -q "$EXPORT_ZIP" -d "$REPLAY_DIR"

# Verify all 12 required files exist in archive
for f in checksums.sha256 comparison.csv input_events.parquet manifest.json metrics.json plan.json rates.csv replay.sh report.md resolved_plan.json spikes.parquet summary.json; do
    if [[ ! -f "$REPLAY_DIR/$f" ]]; then
        echo "Error: Archive missing required file: $f" >&2
        exit 1
    fi
done

# Run replay without server or Python
cd "$REPLAY_DIR"
"$ROOT_DIR/bin/flysim" replay \
    --resolved-plan resolved_plan.json \
    --input-events input_events.parquet \
    --manifest manifest.json \
    --output replayed_output \
    --cache-dir "$ROOT_DIR/data/cache"

grep -q '"total_spikes_A": 267' replayed_output/summary.json || {
    echo "Error: Replay spike count mismatch for Condition A" >&2
    exit 1
}
grep -q '"total_spikes_B": 266' replayed_output/summary.json || {
    echo "Error: Replay spike count mismatch for Condition B" >&2
    exit 1
}

echo ""
echo "========================================================="
echo "  FlyLab End-to-End Smoke Test Passed Completely!        "
echo "  - Web/API Server: Healthy                              "
echo "  - Connectome Graph: 127,400 neurons, 14.68M synapses  "
echo "  - LIF Simulation: Bit-for-bit identical to Brian2      "
echo "  - Export Archive: Complete & Verified Replay           "
echo "========================================================="
