# Experiment 02 — Process Kill Mid-Transaction (v2 rerun with retry)

**Version:** v2
**Failure mode:** infra crash — abrupt process death
**Date:** 2026-05-31
**Duration:** ~10 minutes
**v1 baseline:** experiments/v1/02-process-kill.md

---

## What changed from v1

- Simulator now sends `Idempotency-Key` per intent, retries on 5xx/conn-err up to 3 attempts
- `payment-api` has graceful shutdown (srv.Shutdown 30s on SIGTERM) — but `docker kill` sends SIGKILL, bypassing graceful drain intentionally
- `transfers` table records each execution, enabling replay if a committed txn is retried

## Hypothesis

**Prediction:** Nearly all intents complete eventually. Only intents unlucky enough to exhaust all retries during the full downtime window will fail.

Retry budget: 200ms + 400ms + 800ms ≈ 1.4s of retry window per intent. Kill + restart takes ~10s. Intents that start retrying near the end of the 1.4s window and the app isn't back yet → `intents_failed` (small number, ~10–30).

`replays_served` = 0 expected. Requests committed before kill → 200 on first attempt → no retry. Requests in-flight at kill → Postgres rolls back → retries hit fresh execution, not replay. The replay window (commit + TCP kill mid-send) is theoretically possible but extremely narrow.

---

## Reproduction

```bash
make reset && sleep 10
RETRIES=3 RPS=30 DURATION=60s EXPERIMENT_ID=02 VERSION=v2 make sim &
sleep 10 && docker kill payment-api
docker compose up -d payment-api && sleep 5
wait
make verify
```

---

## Observed

### Simulator summary
```json
{
  "intents_sent":      1799,
  "intents_completed": 1788,
  "intents_failed":    11,
  "replays_served":    0,
  "requests_total":    1838,
  "completed_2xx":     1788,
  "rejected_4xx":      0,
  "failed_5xx":        50,
  "p50_ms": 7,
  "p95_ms": 18,
  "p99_ms": 64,
  "duration_s": 60.70
}
```

### v1 vs v2 comparison

| Metric | v1 (no retry) | v2 (retry=3) |
|--------|-------------|-------------|
| intents_failed | 1554 / 1799 = **86%** | 11 / 1799 = **0.6%** |
| requests_total | 1799 | 1838 (+39 retries) |
| replays_served | — | 0 |

### Verifier output
```
  ✓ I1 ledger sum == 0
  ✓ I2 no balance drift
  ✓     no negative balances
  ✓ I3 all txns have 2 entries
  pass: true
```

---

## Root cause of remaining 11 failures

Retry budget = 200ms + 400ms + 800ms ≈ 1.4s. Kill + Docker restart + healthcheck + app startup ≈ 10s. Any intent that started its retry loop early in the downtime exhausted all 3 attempts before the app came back.

**v2 does not guarantee zero losses on kills longer than the retry budget.** It only guarantees at-most-once execution on retry (idempotency). The retry budget is tunable (`--retries`, `--backoff-base-ms`), but making it arbitrarily long conflicts with end-to-end latency SLAs.

## Why replays_served=0

Three categories of requests during the kill window:

1. **Committed before kill:** got 200 on first attempt → `needRetry=false` → never retried. No replay needed.
2. **In-flight at kill moment:** Postgres WAL rolled back uncommitted txn → retry hits fresh execution → no replay.
3. **After kill, before restart:** connection refused → retry eventually succeeds when app restarts → fresh execution.

The replay path (commit succeeded + TCP connection severed before client received 200 → client retries → finds completed record → replay) is theoretically possible but requires a sub-millisecond window. Did not trigger in this run.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — ledger sum == 0 | ✓ holds | All completed txns have matching debit+credit. |
| I2 — balance drift | ✓ holds | |
| I3 — orphan txn_ids | ✓ holds | Rolled-back txns left no trace. |
| **service availability** | ✓ improved | 0.6% failure vs 86% in v1 |
| **client certainty** | ✓ improved | 1788/1799 intents resolved. 11 still uncertain. |

---

## Lesson preview

v2 reduces lost intents from 86% → 0.6% via retry. The 11 remaining failures show the limit: retry budget (1.4s) < downtime (~10s). Further improvement options:

- **Increase retries / backoff:** reduces loss at cost of higher worst-case latency
- **Persistent retry queue (outbox pattern):** v5 — intents survive restarts, replayed from durable queue
- **Graceful shutdown draining:** protects against SIGTERM (normal `docker stop`), not SIGKILL (`docker kill`)

---

## Reflection

The 86% → 0.6% improvement from just adding a 3-attempt retry is dramatic. Most production payment systems operate at 99.9%+ availability — a single 10s full outage with no retry would create massive customer impact. With retry + idempotency, only clients that happened to exhaust their retry window during extended downtime lose their request.

`replays_served=0` was the most interesting result — no committed txn got retried. In a real system with longer retries or slower DB commits, the replay path would fire more often. That's the scenario idempotency was built for.
