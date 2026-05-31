# v3 Experiments — Index

Two v1 failure mode reruns confirming DB-level fixes. No new binaries, no new services — only DB changes.

## Experiments

| # | Title | Type | Result |
|---|-------|------|--------|
| [03](03-concurrent-race.md) | Concurrent same-payer race (rerun) | rerun | ✓ Fixed — acc_001 never negative across 5/5 attempts. One transfer succeeds, one gets 400 insufficient_funds. |
| [04](04-pool-exhaustion.md) | Pool exhaustion (rerun) | rerun | ✓ Fixed — 42% → 0.1% failure rate at 500 RPS. p99=771ms (queuing) vs instant failures in v1. |

## Key findings

**SELECT FOR UPDATE closes the race completely.** 5 of 5 concurrent-transfer attempts never produced a negative balance. The lock serializes same-payer transfers: second transaction blocks at SELECT, re-reads committed balance, correctly fails the check. Side effect: opposing-direction transfers (acc_001→acc_002 and acc_002→acc_001 simultaneously) can deadlock. Postgres detects and aborts one; 500 is retried successfully.

**SetMaxOpenConns=20 turns connection exhaustion into queuing.** 42% failure rate → 0.1%. The pool queues excess workers instead of opening unbounded connections. Trade-off: p99 rises (771ms vs 17ms) because requests wait for a slot. A delayed success is always better than a fast failure for a payment.

**Two bugs found under load:**
1. `json.RawMessage` NULL scan failure — `response_payload` is NULL for pending rows; `database/sql` can't scan NULL into named `[]byte` type. Fixed: intermediate `[]byte` variable.
2. Deadlock (40P01) — expected consequence of row locking under opposing transfers. Not a data bug. Postgres handles correctly; retry succeeds.

## v1 → v3 improvement summary

| Failure | v1 | v3 |
|---------|----|----|
| Concurrent race (exp 03) | acc_001 = -20000, verifier fails | acc_001 = 40000, verifier passes ✓ |
| Pool exhaustion (exp 04) | 42% failure at 500 RPS | 0.1% failure at 500 RPS ✓ |

## Still open (v4+)

- Deadlock on opposing transfers → lock ordering fix (v4)
- Observability stack (Prometheus + Grafana) → v4
- pg_stat_statements workflow (exp 08/09) → optional v3 extension
