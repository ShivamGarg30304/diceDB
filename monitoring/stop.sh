#!/usr/bin/env bash
# Stop the DiceDB monitoring stack (does NOT touch DiceDB itself).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for proc in "grafana server" "prometheus --config.file=$HERE/prometheus.yml" "redis_exporter -redis.addr"; do
  pids=$(pgrep -f "$proc" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    echo "• stopping: $proc  (pids: $pids)"
    kill $pids 2>/dev/null || true
  fi
done
echo "done."
