#!/bin/sh
# Fake `flysim` for interpretation API tests. `digest` copies FAKE_FLYSIM_GRAPH to --output
# (or fails with FAKE_FLYSIM_FAIL set) after FAKE_FLYSIM_SLEEP seconds (if set); every invocation
# is appended to FAKE_FLYSIM_LOG.
if [ -n "$FAKE_FLYSIM_LOG" ]; then
  printf 'CALL:%s\n' "$*" >> "$FAKE_FLYSIM_LOG"
fi
[ "$1" = "digest" ] || { echo "fake flysim: unsupported command $1" >&2; exit 2; }
shift
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --resolved-plan|--spikes|--manifest|--cache-dir) [ -e "$2" ] || { echo "fake flysim: $1 $2 does not exist" >&2; exit 3; }; shift 2 ;;
    *) echo "fake flysim: unknown flag $1" >&2; exit 2 ;;
  esac
done
if [ -n "$FAKE_FLYSIM_SLEEP" ]; then
  sleep "$FAKE_FLYSIM_SLEEP"
fi
if [ -n "$FAKE_FLYSIM_FAIL" ]; then
  echo "$FAKE_FLYSIM_FAIL" >&2
  exit 1
fi
cp "$FAKE_FLYSIM_GRAPH" "$out"
