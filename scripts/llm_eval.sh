#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "=== FlyLab: LLM Parsing Evaluation Suite ==="

# Calls the real planner (claude -p) through the API; each prompt costs one Claude request.
PORT=${EVAL_PORT:-8091}
WORK_ROOT="${SMOKE_WORK_DIR:-$ROOT_DIR/.smoke}"
mkdir -p "$WORK_ROOT"
TEMP_DIR=$(mktemp -d "$WORK_ROOT/llm_eval_XXXXXX")
SERVER_PID=""
trap '[[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true; rm -rf "$TEMP_DIR"' EXIT

HOST=127.0.0.1 PORT="$PORT" DB_PATH="$TEMP_DIR/eval.db" ARTIFACTS_DIR="$TEMP_DIR/artifacts" REGISTRATION_OPEN=true \
    "$ROOT_DIR/bin/flylab" > "$TEMP_DIR/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server
for i in {1..20}; do
    if curl -s "http://127.0.0.1:$PORT/health" | grep -q '"status":"ok"'; then
        break
    fi
    sleep 0.5
done

EVAL_USER="eval_$(date +%s)_$RANDOM"
REG_RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/v1/auth/register" -H "Content-Type: application/json" \
    -d "{\"username\":\"$EVAL_USER\",\"password\":\"eval-pass-$RANDOM$RANDOM\"}")
TOKEN=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["token"])' "$REG_RESP") || {
    echo "Error: registration failed: $REG_RESP" >&2; exit 1; }

evaluate_prompt() {
    local prompt="$1"
    local expected_status="$2"
    local desc="$3"

    echo -n "Testing [$desc]... "
    local t0
    t0=$(perl -MTime::HiRes=time -e 'printf "%d\n", time*1000')

    local resp
    resp=$(curl -s -X POST "http://127.0.0.1:$PORT/api/v1/plans/parse" \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d "{\"prompt\":\"$prompt\",\"dataset_id\":\"flywire_630\",\"report_language\":\"en\"}")

    local t1
    t1=$(perl -MTime::HiRes=time -e 'printf "%d\n", time*1000')
    local elapsed_ms=$(( t1 - t0 ))

    local status source
    status=$(echo "$resp" | grep -o '"status":"[^"]*' | head -n1 | cut -d'"' -f4)
    source=$(echo "$resp" | grep -o '"source":"[^"]*' | head -n1 | cut -d'"' -f4)
    if [[ "$source" != "claude" ]]; then
        echo "FAIL (planner source is '$source', not claude)"
        echo "Response: $resp" >&2
        return 1
    fi

    if [[ "$status" == "$expected_status" ]]; then
        echo "PASS ($status in ${elapsed_ms}ms)"
    else
        echo "FAIL (expected $expected_status, got $status)"
        echo "Response: $resp" >&2
        return 1
    fi
}

echo "Running prompt test cases..."
evaluate_prompt "Stimulate sugar_grn at 50 Hz for 100 ms and readout mn9" "ready" "Single Stimulus"
evaluate_prompt "Compare sugar_grn 50 Hz with and without demo_silencing for 100 ms, readout mn9" "ready" "Compare Silencing"
evaluate_prompt "Turn off inhibitory neurons and see what happens" "needs_input" "Ambiguous Input"
evaluate_prompt "Show me how the fly will walk after removing these neurons" "unsupported" "Unsupported Organism Walking"
evaluate_prompt "Using FlyWire data, stimulate bitter GRNs at 120 Hz for 300 ms and read out MN9" "ready" "FlyWire mention is not out of scope"
evaluate_prompt "Сравни активацию сахарных рецепторов 80 Гц с подавлением demo_silencing, 200 мс" "ready" "Russian compare silencing"

echo ""
echo "=== All LLM Evaluation Test Cases Passed ==="
