# Experiment 04 — DB Connection Pool Exhaustion

**Version:** v1
**Failure mode:** capacity — unbounded connection pool
**Date:** 2026-05-24
**Duration:** ~10 minutes

---

## Hypothesis

### Q1 — What happens to requests once connections are exhausted?

**Prediction: Immediate 500 errors — no queuing, no waiting.**

v1 sets `MaxOpenConns=0` (Go's default = unlimited). At 200 workers, Go's `database/sql` pool tries to open up to 200 TCP connections to Postgres. Postgres default `max_connections=100`. When connection 101+ arrives:

1. Postgres immediately rejects: `FATAL: sorry, too many clients already`
2. Go gets that error on the connect attempt
3. SQL query fails before execution
4. Handler catches the DB error → returns 500
5. Request fails instantly — no retry, no queue, no wait

Requests that land on already-open connections succeed. Requests that need a new connection when Postgres is saturated fail fast with 500.

### Q2 — What error appears in payment-api logs?

**Prediction:** structured log event `transfer.failed` at error level with:

```json
{
  "event": "transfer.failed",
  "error": "FATAL: sorry, too many clients already (SQLSTATE 53300)"
}
```

pgx wraps the Postgres FATAL message. The SQLSTATE 53300 = "too many connections."

### Q3 — Does DB state stay consistent? Will verifier pass?

**Prediction: Yes, DB consistent. Verifier passes.**

Failed requests never established a connection → no transaction started → no partial state possible. Only successfully connected requests ran transactions (fully ACID). Postgres rolled back any in-flight txns if connections died.

v1 has **zero retry logic** — failed requests are dropped. No eventual consistency behaviour. No queue. Requests fail, clients get 500, DB is untouched by those requests.

### Q4 — Difference between `max_connections` and `MaxOpenConns`

Two different layers, both about DB connections (NOT HTTP):

| Setting | Layer | What it controls |
|---------|-------|-----------------|
| `max_connections=100` | Postgres server | Total TCP connections across ALL clients combined. Connection 101 → `FATAL: sorry, too many clients`. Set in postgresql.conf. |
| `SetMaxOpenConns(N)` | Go `database/sql` pool | Max DB connections THIS app will open. When all N in use → excess queries **wait in Go pool queue** (backpressure). Default=0=unlimited. |

**Why v1 breaks:**
```
MaxOpenConns=0 → 200 workers try to open 200 connections
Postgres cap=100 → rejects connections 101–200
→ 100 workers get FATAL error → 500 to caller
```

**v3 fix:**
```go
dbx.SetMaxOpenConns(20)   // cap connections at 20
dbx.SetMaxIdleConns(10)   // keep 10 warm
```
Now 200 workers compete for 20 slots. Overflow waits in Go's pool queue, not at Postgres. Latency rises under load but no connection errors. Controlled backpressure.

---

## Reproduction

### Pre-conditions
- Fresh DB state (`make reset && sleep 10`)
- Stack healthy

### Steps

```bash
# Crank way past Postgres max_connections
EXPERIMENT_ID=04 RPS=500 WORKERS=200 DURATION=30s make sim

# Check error rate in summary
# Then check logs for error type
docker logs payment-api 2>&1 | grep '"level":"ERROR"' | head -5 | jq .

# Verify DB consistency
make verify
```

---

## Observed

### Simulator summary
```json
{
  "sent": 14809,
  "completed_2xx": 8611,
  "rejected_4xx": 0,
  "failed_5xx": 6198,
  "p50_ms": 1,
  "p95_ms": 223,
  "p99_ms": 3281,
  "actual_rps": 473.19,
  "duration_s": 31.30
}
```

- **8611** (58%) succeeded
- **6198** (42%) failed with 500 — connection rejected by Postgres
- p50=1ms is misleading (instant failures pull median down — same pattern as experiment 02)
- p99=3281ms — successful requests got very slow under pressure (connections recycled slowly)

### Error in logs — exact match
```json
{
  "msg": "transfer.failed",
  "error": "begin: failed to connect to `user=payment database=payment`: 172.27.0.2:5432 (postgres): server error: FATAL: sorry, too many clients already (SQLSTATE 53300)"
}
```

Error fires at `begin:` — meaning `BeginTx()` couldn't even establish a connection. Failure happens before any SQL runs.

### Verifier output
```
  ✓ I1 ledger sum == 0
  ✓ I2 no balance drift
  ✓     no negative balances
  ✓ I3 all txns have 2 entries
  pass: true
```
All invariants pass. DB consistent.

---

## Root cause

`db.go` sets no pool limits:
```go
// v1: deliberately no pool tuning
// MaxOpenConns=0 (unlimited), MaxIdleConns=2
```

At 200 workers, Go's pool tries to open a new TCP connection per concurrent request. Postgres `max_connections=100` (default in `postgres:16-alpine`). When the 101st connection attempt arrives, Postgres returns `FATAL: sorry, too many clients already (SQLSTATE 53300)`. pgx wraps this as an error on the `BeginTx()` call → handler hits `if err != nil` at the begin step → `transfer.failed` logged → 500 returned.

No retry. No queue. Request dropped immediately.

Successful requests also degraded: p99 hit 3281ms because connections were being saturated, held long, and recycled slowly — adding wait time even for requests that eventually got a slot.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — `SUM(ledger) == 0` | ✓ holds | Failed requests never started a transaction. No partial entries. |
| I2 — balance matches ledger | ✓ holds | Only fully committed transfers visible. |
| I3 — all txn_ids have 2 rows | ✓ holds | No orphans — failed requests left zero trace. |
| app — no negative balance | ✓ holds | Race condition not triggered here (different experiment). |
| **capacity — service availability** | ✗ broken | 42% of requests dropped. System partially unavailable at realistic load. |

DB integrity perfect. Service reliability broken — 4 in 10 requests returned 500.

---

## Lesson preview

**Broke because:** `MaxOpenConns=0` (unlimited) — Go pool tries to open a new connection per concurrent request. Postgres rejects beyond `max_connections=100`. No backpressure, no queue — immediate 500.

**Fixed in: v3** — `SetMaxOpenConns(20)` + `SetMaxIdleConns(10)` + `SetConnMaxLifetime(5*time.Minute)`. Pool queues excess requests instead of opening new connections. Postgres sees bounded load. Also: pgbouncer as connection proxy optional for higher scale.

---

## Reflection

The failure point was earlier than expected — `BeginTx()`, not inside a query. The connection is the bottleneck, not the SQL. This means even the simplest possible query (no joins, no aggregations) would fail the same way — the problem is purely capacity, not query complexity.

The p99=3281ms on successful requests is worth noting. DB connection pressure degrades latency for everyone, not just the failed requests. A 42% error rate AND 3x latency increase for the survivors — capacity problems cascade.

The p50=1ms trap appeared again: mixing fast failures with slow successes produces a misleading median. Always segment latency by status code when diagnosing capacity issues.

Core takeaway: `MaxOpenConns=0` = no backpressure = Postgres absorbs unbounded connection attempts = predictable failure at load. `SetMaxOpenConns(20)` would have caused requests to queue in Go's pool instead, capping Postgres load and raising latency gracefully rather than failing outright.
