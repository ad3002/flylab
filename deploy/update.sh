#!/usr/bin/env bash
# Run as root: pulls, rebuilds as user flylab, restarts the service, checks health.
set -euo pipefail

APP=/mnt/beta/flylab/app

runuser -u flylab -- env HOME=/mnt/beta/flylab PATH="/mnt/beta/flylab/.cargo/bin:$PATH" \
    bash -c "cd '$APP' && git pull --ff-only && make setup"

systemctl restart flylab

for _ in $(seq 1 30); do
    health=$(curl -s http://127.0.0.1:8107/health || true)
    if [[ "$health" == *'"status":"ok"'* ]]; then
        echo "$health"
        exit 0
    fi
    sleep 1
done

echo "ERROR: flylab did not report status ok on http://127.0.0.1:8107/health within 30 s (last response: '${health:-<none>}')" >&2
exit 1
