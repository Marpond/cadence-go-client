# Fanout Stress Tests

Quick reference for running performance comparisons.

## Setup

```bash
# Start everything (Cassandra takes ~30s to init)
docker compose up -d

# Build
go build -o fanout-stress ./x/fanout/stresstest/cmd
```

## Running Tests

```bash
# Basic comparison
./fanout-stress -items=500 -work-ms=10

# With heartbeats (this is where things get interesting)
./fanout-stress -items=1000 -work-ms=500 -heartbeat

# Scale test - 5000 items, 200ms work, heartbeats on
./fanout-stress -items=5000 -work-ms=200 -heartbeat
```

## Key Flags

| Flag | Default | Notes |
|------|---------|-------|
| `-items` | 100 | Number of activities to schedule |
| `-work-ms` | 10 | Simulated activity duration |
| `-heartbeat` | false | Enable 100ms heartbeat interval |
| `-report` | - | Save JSON results to file |

## What Gets Compared

1. **naive-all-at-once** — Schedule everything immediately (anti-pattern)
2. **naive-with-concurrency-ctrl** — Manual semaphore, 50 concurrent
3. **fanout-in-place** — API with BatchFuture, 50 concurrent
4. **fanout-with-children** — API with child workflow distribution

## Important Notes

**NUM_HISTORY_SHARDS**: Default is 16 in docker-compose.yml. For meaningful child workflow results, bump to 256. This requires `docker compose down -v` to reinitialize Cassandra.

**Heartbeats are the key**: Without `-heartbeat`, naive often wins at small scale. With heartbeats, the hot shard problem manifests quickly.

**Child fanout threshold**: Set to 100 items in runner.go. 500 items → 5 children × 50 concurrent = 250 effective parallelism.

## Dashboards

- Cadence Web: http://localhost:8088
- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000

## Cleanup

```bash
docker compose down -v  # -v clears Cassandra data
```
