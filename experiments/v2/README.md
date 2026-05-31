# v2 Experiments — Index

Five experiments: three reruns of v1 failures (to confirm fixes), two new tests of v2-specific logic.

## Baseline

Before v2 experiments: 30s @ 50 RPS with Idempotency-Key — all requests complete, all invariants pass.

## Experiments

| # | Title | Type | Pass criteria | Result |
|---|-------|------|---------------|--------|
| [01](../v1/01-duplicate-request.md) | Duplicate request (v2 rerun) | rerun | Second call replays with same txn_id + `Idempotency-Replay: true`. acc_001 charged once. | ✓ **Fixed** |
| [02](02-process-kill.md) | App kill mid-txn (w/ retry) | rerun | intents_failed near 0. requests_total > intents_sent (retries fired). Verifier passes. | ✓ **0.6% failure** (was 86%) |
| [05](05-postgres-restart.md) | Postgres restart (w/ retry) | rerun | intents_failed near 0. Verifier passes. | ✓ **5.8% failure** (was 14.7%) |
| [06](06-payload-conflict.md) | Payload conflict | **new** | Same key + different payload → 422 `idempotency_key_conflict`. Only first payload executes. | ✓ **422 returned** |
| [07](07-concurrent-same-key.md) | Concurrent same key | **new** | 5 concurrent POSTs → 1 executes, others get 409. acc_001 charged once. | ⚠️ **1×200, 4×500 (bug found + fixed)** |

## Key findings

**Retry (exp 02/05) dramatically reduces lost intents.** 86% → 0.6% on app kill, 14.7% → 5.8% on DB restart. Remaining failures are intents that exhausted retries during extended downtime (> 1.4s retry budget).

**replays_served=0 in all retry experiments.** Requests that committed before crash got 200 and didn't retry. In-flight requests at crash time had txns rolled back → retries re-executed fresh. The replay path requires commit + TCP tear before response is received — theoretically possible, didn't trigger.

**422 is the right code for payload conflicts (exp 06).** 409 = transient (wait and retry). 422 = permanent caller bug (fix your code). The distinction matters for automated retry logic.

**DB constraint = last line of defense (exp 07).** `isUniqueViolation()` had a bug (pgx/v5 stdlib wrapping) — returned false → clients got 500 instead of 409. But the UNIQUE constraint on `transfers.idempotency_key` still blocked the double INSERT at DB level. acc_001 charged once despite the app bug. Fixed: added `strings.Contains(err.Error(), "23505")` fallback.

## v1 → v2 improvement summary

| Failure | v1 result | v2 result |
|---------|-----------|-----------|
| Duplicate request | Double charge (acc_001 = 0) | Single charge (acc_001 = 50000) ✓ |
| App kill (86%→) | 1554/1799 intents lost | 11/1799 intents lost ✓ |
| Postgres restart (14.7%→) | 264/1799 intents lost | 95/1643 intents lost ✓ |
| Same-key concurrent | N/A in v1 | 1 execution, charge protection holds ✓ |
| Payload conflict detection | N/A in v1 | 422 + no execution ✓ |

## Still broken (v3)

- Race condition (exp 03) — `SELECT FOR UPDATE` not added. v3 lesson.
- Pool exhaustion (exp 04) — `MaxOpenConns` not set. v3 lesson.
