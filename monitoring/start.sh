#!/usr/bin/env bash
# Start the DiceDB monitoring stack: redis_exporter -> Prometheus -> Grafana.
# All runtime state (PIDs, logs, TSDB, grafana db) lives under monitoring/.run.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN="$HERE/.run"
mkdir -p "$RUN/prometheus" "$RUN/grafana" "$RUN/logs"

DICE_ADDR="${DICE_ADDR:-localhost:7379}"   # where DiceDB listens
EXPORTER_PORT="${EXPORTER_PORT:-9121}"
PROM_PORT="${PROM_PORT:-9090}"
GRAFANA_PORT="${GRAFANA_PORT:-3000}"

GRAFANA_HOME="$(brew --prefix grafana)/share/grafana"

# Keep the provisioned dashboard path correct even if the repo moves.
cat > "$HERE/grafana/provisioning/dashboards/provider.yml" <<YAML
apiVersion: 1
providers:
  - name: DiceDB
    type: file
    disableDeletion: false
    allowUiUpdates: true
    options:
      path: $HERE/grafana/dashboards
      foldersFromFilesStructure: false
YAML

is_up() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

start() { # name port logfile cmd...
  local name="$1" port="$2" log="$3"; shift 3
  if is_up "$port"; then
    echo "• $name already listening on :$port — skipping"
    return
  fi
  echo "• starting $name on :$port"
  ("$@" >"$log" 2>&1 &) ; sleep 0.3
  pgrep -nf "$1" >/dev/null 2>&1 || true
}

# 1) redis_exporter — single-target mode against DiceDB
start "redis_exporter" "$EXPORTER_PORT" "$RUN/logs/redis_exporter.log" \
  redis_exporter \
  -redis.addr "redis://$DICE_ADDR" \
  -web.listen-address ":$EXPORTER_PORT"

# 2) Prometheus
start "prometheus" "$PROM_PORT" "$RUN/logs/prometheus.log" \
  prometheus \
  --config.file="$HERE/prometheus.yml" \
  --storage.tsdb.path="$RUN/prometheus" \
  --web.listen-address=":$PROM_PORT"

# 3) Grafana
start "grafana" "$GRAFANA_PORT" "$RUN/logs/grafana.log" \
  grafana server \
  --homepath "$GRAFANA_HOME" \
  cfg:default.server.http_port="$GRAFANA_PORT" \
  cfg:default.paths.data="$RUN/grafana" \
  cfg:default.paths.logs="$RUN/logs" \
  cfg:default.paths.provisioning="$HERE/grafana/provisioning"

echo
echo "Stack up:"
echo "  exporter   http://localhost:$EXPORTER_PORT/metrics"
echo "  prometheus http://localhost:$PROM_PORT"
echo "  grafana    http://localhost:$GRAFANA_PORT   (admin / admin)"
echo "  dashboard  http://localhost:$GRAFANA_PORT/d/dicedb-keyspace"
echo
echo "Make sure DiceDB is running:  go run main.go   (listens on $DICE_ADDR)"
echo "Logs: $RUN/logs/   |  stop with: $HERE/stop.sh"
