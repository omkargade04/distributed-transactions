# Experiment 05 — Postgres Restart Mid-Load (v2 rerun with retry)

**Version:** v2
**Failure mode:** infra crash — DB process restart
**Date:** 2026-05-31
**v1 baseline:** experiments/v1/05-postgres-restart.md

---

## What changed from v1

Same as exp 02: simulator retries with idempotency key. Postgres restart uses `docker compose stop` + `docker compose up` to avoid Docker Desktop port-binding issue.

---

## Observed

### Simulator summary
```json
{
  "intents_sent":      1643,
  "intents_completed": 1548,
  "intents_failed":    95,
  "replays_served":    0,
  "requests_total":    1845,
  "completed_2xx":     1548,
  "rejected_4xx":      0,
  "failed_5xx":        297,
  "p50_ms": 7,
  "p95_ms": 55,
  "p99_ms": 351,
  "duration_s": 60.87,
  "actual_intents_rps": 26.99
}
```

### v1 vs v2 comparison

| Metric | v1 (no retry) | v2 (retry=3) |
|--------|-------------|-------------|
| intents_failed | 264/1799 = **14.7%** | 95/1643 = **5.8%** |
| requests_total | 1799 | 1845 (+202 retries) |
| p99_ms | 45 | 351 (retries add latency) |

### Verifier
```
  ✓ I1 ledger sum == 0
  ✓ I2 no balance drift
  ✓     no negative balances
  ✓ I3 all txns have 2 entries
  pass: true
```

---

## Analysis

### Why 5.8% failures vs 0.6% in exp 02

Postgres restart takes longer than app kill+restart:

| Step | Duration |
|------|---------|
| `docker compose stop postgres` | ~0.6s |
| 8s intentional wait | 8s |
| `docker compose up postgres` | ~0.5s |
| DNS re-registration gap | ~1-2s |
| Postgres healthcheck (pg_isready) | ~2s |
| **Total downtime** | ~12-13s |

App kill+restart in exp 02 = ~5s downtime. Retry budget (1.4s) covers more of it.
Postgres restart = ~13s. Retry budget of 1.4s covers only the edges. 95 intents exhausted all retries during the core downtime window.

### Why p99=351ms

Retries add latency. An intent that required 2 retries at 200ms + 400ms backoff adds ~600ms on top of the actual request latency. p99 captures the worst cases: 3 attempts × backoff → high tail latency.

### Why intents_sent=1643 not ~1800

Effective RPS dropped to 26.99 (vs target 30). Retries consume worker capacity — workers busy retrying can't pick up new jobs from the ticker. Effective throughput decreases during high-retry periods.

### Why replays_served=0

Same reason as exp 02: requests that committed before Postgres stopped got 200 and weren't retried. Requests in-flight at stop moment had txns rolled back by Postgres → retries execute fresh. No committed-before-outage + retried scenario observed.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1/I2/I3 | ✓ holds | DB consistent — only committed txns visible |
| **service availability** | ✓ improved | 5.8% failure vs 14.7% in v1 |
| **client certainty** | ✓ improved | 1548/1643 intents resolved |

---

## Lesson

v2 retry helps here (14.7% → 5.8%) but less dramatically than exp 02 because Postgres restart is slower. The failure floor is set by `retry_budget / downtime_duration`. To reduce further:

- More retries with longer max backoff (cost: higher p99)
- Persistent outbox queue (v5) — intents survive any outage duration
- Separate read/write DB (v6) — read replicas stay up during primary restart
