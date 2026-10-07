#!/bin/sh
# Fake `claude` CLI for tests. Behaviour is selected with FAKE_CLAUDE_MODE; every argument and
# the stdin prompt are appended to FAKE_CLAUDE_LOG (if set) so tests can check the invocation.
if [ -n "$FAKE_CLAUDE_LOG" ]; then
  for a in "$@"; do printf 'ARG:%s\n' "$a" >> "$FAKE_CLAUDE_LOG"; done
fi
prompt=$(cat)
if [ -n "$FAKE_CLAUDE_LOG" ]; then
  printf 'STDIN:%s\n' "$prompt" >> "$FAKE_CLAUDE_LOG"
fi
if [ -n "$FAKE_CLAUDE_DELAY" ]; then
  sleep "$FAKE_CLAUDE_DELAY"
fi

ok_envelope() {
  printf '{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.0123,"result":"","structured_output":%s}\n' "$1"
}

case "${FAKE_CLAUDE_MODE:-ready_single}" in
  ready_single)
    ok_envelope '{"status":"ready","message":"Stimulate sugar GRNs at 80 Hz for 200 ms, read out MN9.","unresolved_fields":[],"plan":{"experiment_type":"single","activation":[{"group_id":"sugar_grn","rate_hz":80}],"silencing":[],"readout":[{"group_id":"mn9"}],"duration_ms":200,"repeats":2,"base_seed":7}}'
    ;;
  ready_compare)
    ok_envelope '{"status":"ready","message":"Compare sugar GRN drive with and without demo_silencing.","unresolved_fields":[],"plan":{"experiment_type":"compare_silencing","activation":[{"group_id":"sugar_grn","rate_hz":100}],"silencing":[{"group_id":"demo_silencing"}],"readout":[{"group_id":"mn9"}],"duration_ms":100,"repeats":1,"base_seed":42}}'
    ;;
  needs_input)
    ok_envelope '{"status":"needs_input","message":"Which neurons should be stimulated?","unresolved_fields":["activation.selector"]}'
    ;;
  unsupported)
    ok_envelope '{"status":"unsupported","message":"Walking is whole-animal behaviour; FlyLab simulates spiking only.","unresolved_fields":[]}'
    ;;
  is_error)
    printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"duration_ms":10,"total_cost_usd":0,"result":"API Error: 529 overloaded"}'
    ;;
  is_error_exit)
    printf '%s\n' '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"model refused"}'
    exit 1
    ;;
  malformed)
    ok_envelope '"this is not an object"'
    ;;
  bad_status)
    ok_envelope '{"status":"maybe","message":"?","unresolved_fields":[]}'
    ;;
  missing_output)
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"duration_ms":5,"total_cost_usd":0.001,"result":"I could not produce JSON"}'
    ;;
  invalid_plan)
    ok_envelope '{"status":"ready","message":"Run 5 seconds.","unresolved_fields":[],"plan":{"experiment_type":"single","activation":[{"group_id":"sugar_grn","rate_hz":50}],"silencing":[],"readout":[{"group_id":"mn9"}],"duration_ms":5000,"repeats":1,"base_seed":42}}'
    ;;
  long_message)
    # 501 characters: one over the v4 cap of the planner message.
    ok_envelope "{\"status\":\"needs_input\",\"message\":\"$(printf 'x%.0s' $(seq 1 501))\",\"unresolved_fields\":[]}"
    ;;
  too_many_fields)
    ok_envelope '{"status":"needs_input","message":"Which?","unresolved_fields":["a","b","c","d","e","f","g","h","i","j","k"]}'
    ;;
  unknown_ids)
    # Root ids the user typed that are not in the v630 connectome (plus one real one).
    ok_envelope '{"status":"ready","message":"Stimulate the given neurons at 50 Hz.","unresolved_fields":[],"plan":{"experiment_type":"single","activation":[{"neuron_ids":["720575940000000001","720575940620900446","720575940999999999"],"rate_hz":50}],"silencing":[],"readout":[{"group_id":"mn9"}],"duration_ms":100,"repeats":1,"base_seed":42}}'
    ;;
  from_file)
    # Interpretation tests: structured_output is the JSON in FAKE_CLAUDE_OUTPUT_FILE.
    ok_envelope "$(cat "$FAKE_CLAUDE_OUTPUT_FILE")"
    ;;
  garbage)
    printf 'Segmentation fault\n'
    ;;
  nonzero)
    printf 'authentication failed: please run /login\n' >&2
    exit 2
    ;;
  sleep)
    exec sleep "${FAKE_CLAUDE_SLEEP:-5}"
    ;;
  *)
    printf 'unknown FAKE_CLAUDE_MODE %s\n' "$FAKE_CLAUDE_MODE" >&2
    exit 3
    ;;
esac
