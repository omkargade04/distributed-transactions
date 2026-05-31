# v1 Experiments — Index

Five deliberate crash experiments. Each breaks the system in a different way, documents the failure, and previews the v2–v3 fix.

## Baseline

Before any experiments: 30s @ 50 RPS — 1499 requests, 100% 2xx, p50=4ms, p99=76ms. All invariants pass.

## Experiments

| # | Title | Failure category | Verifier | Invariant broken | Fix version |
|---|-------|-----------------|---------|-----------------|-------------|
| [01](01-duplicate-request.md) | Duplicate request | app bug | ✓ pass | app: no negative, BUT client double-charged | v2 — idempotency keys |
| [02](02-process-kill.md) | App kill mid-txn | infra crash | ✓ pass | none (DB) — client certainty broken | v2 — retry + outbox |
| [03](03-concurrent-race.md) | Concurrent same-payer race | race condition | ✗ **fail** | `balance_minor < 0` (acc_001 = -$200) | v3 — SELECT FOR UPDATE |
| [04](04-pool-exhaustion.md) | DB pool exhaustion | capacity | ✓ pass | none (DB) — 42% requests 500 at 500 RPS | v3 — SetMaxOpenConns |
| [05](05-postgres-restart.md) | Postgres restart mid-load | infra crash | ✓ pass | none (DB) — 14.7% requests 500, DNS gap | v2 — retry + backoff |

## Key findings

**ACID ≠ application correctness.** Experiments 01, 02, 04, 05 all had verifier pass. DB was perfectly consistent in every case except 03. The failures were at the application layer (missing idempotency, no retry, no backpressure) — invisible to structural invariant checks.

**The race is the only DB-layer bug.** Experiment 03 is the only one where `make verify` exited 1. READ COMMITTED + no row lock = lost update under concurrent load. Triggered on attempt 1.

**Three error phases in exp 05.** Postgres graceful shutdown (57P01) → connection refused → Docker DNS gap. The DNS gap (`no such host`) is container-specific — doesn't exist in native deployments.

**p50 latency is a lie under mixed load.** Experiments 02, 04, 05 all showed p50=1ms or similar because instant connection failures pulled the median down. Always segment latency by status code.

## Verifier invariants

```sql
-- I1: ledger sum must equal zero
SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries;

-- I2: each account's balance must match initial + sum(ledger)
SELECT a.id FROM accounts a
LEFT JOIN ledger_entries l ON l.account_id = a.id
GROUP BY a.id, a.balance_minor
HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + 100000;

-- I3: every txn_id must have exactly 2 rows (1 debit + 1 credit)
SELECT txn_id FROM ledger_entries GROUP BY txn_id HAVING COUNT(*) != 2;

-- App rule: no negative balances
SELECT id FROM accounts WHERE balance_minor < 0;
```

## Data files

JSONL simulator outputs stored in `data/` (gitignored):
- `00-sim-requests.jsonl` — baseline happy path (50 RPS, 30s)
- `02-sim-requests.jsonl` — app kill run (30 RPS, 60s)
- `04-sim-requests.jsonl` — pool exhaustion (500 RPS, 30s)
- `05-sim-requests.jsonl` — Postgres restart (30 RPS, 60s)
