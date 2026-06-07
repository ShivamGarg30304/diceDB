# DiceDB Monitoring (redis_exporter → Prometheus → Grafana)

Local observability stack to watch the DiceDB keyspace (number of keys over time).

```
DiceDB (:7379)  --INFO-->  redis_exporter (:9121)  --scrape-->  Prometheus (:9090)  --query-->  Grafana (:3000)
```

`redis_exporter` runs DiceDB's `INFO` command and parses the `# Keyspace`
section (`db0:keys=N,...`) into the `redis_db_keys{db="db0"}` metric.

## Prerequisites (already installed)

```bash
brew install prometheus grafana            # core formulae
# redis_exporter binary in /opt/homebrew/bin (from github.com/oliver006/redis_exporter releases)
```

## Run

```bash
# 1. start DiceDB
go run main.go            # listens on localhost:7379

# 2. start the monitoring stack
./monitoring/start.sh

# 3. stop the stack (leaves DiceDB alone)
./monitoring/stop.sh
```

| Service        | URL                                            |
|----------------|------------------------------------------------|
| Exporter       | http://localhost:9121/metrics                  |
| Prometheus     | http://localhost:9090                          |
| Grafana        | http://localhost:3000  (admin / admin)         |
| Dashboard      | http://localhost:3000/d/dicedb-keyspace        |

## Key metrics

- `redis_db_keys{db="db0"}` — current number of keys (the main one you asked for)
- `redis_up` — 1 if the exporter can reach DiceDB (`PING` → `PONG`)
- `deriv(redis_db_keys{db="db0"}[1m])` — rate of key growth/eviction

## Notes / gotchas

- The exporter reports `db0..db15` because it assumes 16 standard Redis DBs;
  DiceDB only populates `db0`, so the rest stay `0`. That's expected.
- DiceDB's `INFO` ignores its arguments and always returns just the keyspace
  section, so memory/CPU/command-rate panels won't have data — only keyspace
  metrics are meaningful here.
- Runtime state (TSDB, Grafana DB, logs, PIDs) lives in `monitoring/.run/`
  which is git-ignored. Delete it to reset.
- Override ports/target via env: `DICE_ADDR=localhost:7379 PROM_PORT=9090 ./monitoring/start.sh`.
