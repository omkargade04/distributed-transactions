# v1 Reflection

## What we built

Single Go binary, Postgres, Docker Compose. Three endpoints. No auth, no idempotency, no retries, no pool tuning, no indexes beyond PK. Deliberately primitive — every missing piece is a lesson in v2–v3.

## What surprised me

**ACID is stronger than expected at the DB layer.** Every crash experiment except the race left the DB perfectly consistent. Postgres rolled back uncommitted transactions automatically on process death, connection drop, and shutdown. The WAL doesn't need your help — it just works.

**The race was easier to trigger than expected.** Experiment 03 triggered on attempt 1, not attempt 15+. Under concurrent load, READ COMMITTED + no row lock = near-certain collision. Not an edge case. This is why production payment systems always use row locks — it's not defensive programming, it's mandatory.

**The verifier passing is the dangerous result.** Four out of five experiments had all invariants pass while the system was fundamentally broken (double-charging, silent 500s, lost payment intents). Mathematical consistency of the ledger is necessary but far from sufficient. An audit tool that checks structure cannot detect business-rule violations.

**Docker DNS gap.** Experiment 05 surfaced a failure mode that doesn't exist outside containers: Docker's overlay DNS doesn't immediately re-register a container when it restarts. App couldn't resolve `postgres` hostname for ~1-2s after the container came back. Native deployments don't have this. Something to design around in containerized prod environments.

**p50 latency is a lie.** Instant connection failures (status=0, latency=1ms) mixed with real request latencies produced misleading p50s in experiments 02, 04, 05. Always filter by status code before reading latency percentiles.

## What was harder than expected

**Getting the race to show as VERIFIER FAILURE specifically.** Initially thought I1 or I2 would catch it. They don't — both pass even with a negative balance, because the ledger correctly records both debits. Had to understand that the verifier's negative-balance check is an app-level rule, not a structural invariant.

**Docker `docker restart` port binding issue.** `docker restart postgres` failed with port binding errors on Docker Desktop Mac. Had to use `docker compose stop` + `docker compose up` instead. Real-world friction with containerized environments that specs don't anticipate.

## Core lessons from v1

1. **ACID atomicity** = all-or-nothing within a transaction. App process can die mid-flight, Postgres cleans up. This is not optional behavior — it's guaranteed by the WAL.

2. **Idempotency gap** = no deduplication → duplicate requests → silent double-charge. Verifier can't detect this. Only business-level audit catches it. Fix: idempotency_key + UNIQUE constraint.

3. **Lost update** = READ COMMITTED + no FOR UPDATE = two transactions read same value, both write based on it. Only one should win. Fix: SELECT FOR UPDATE (pessimistic) or version column + retry (optimistic).

4. **Connection pool backpressure** = MaxOpenConns=0 + high concurrency → Postgres saturated → 42% failure rate. Fix: SetMaxOpenConns(20) — queue at Go pool, not at Postgres.

5. **Request certainty** = app crash or DB restart → 500 to caller. Client doesn't know if payment landed. Fix: idempotency key + client retry with backoff. Server deduplicates on retry.

## What v2 needs to fix

From experiments 01, 02, 05 — all share the same root cause at different triggers:

```
Client sent request → got 500/connection error → doesn't know if payment landed
```

v2 fix:
1. Add `idempotency_key` (UUID) to transfer request
2. Add `transfers` table with `UNIQUE(idempotency_key)`
3. On duplicate key: return cached result, no second execution
4. Client retries with same key on any 5xx → safe retry
5. Add graceful shutdown: `signal.Notify + srv.Shutdown(ctx)` — drain in-flight requests before dying

From experiment 03:

```
Two concurrent debits from same account → both see stale balance → both commit → negative balance
```

v3 fix:
1. Add `SELECT balance_minor FROM accounts WHERE id = $1 FOR UPDATE` in transfer.go
2. Second transaction blocks until first commits
3. Re-reads committed balance → correctly fails balance check

## What v3 needs to fix

From experiment 04:

```
MaxOpenConns=0 + 200 workers → Postgres sees 200 connection attempts → rejects 101+ → 42% 500s
```

v3 fix:
1. `dbx.SetMaxOpenConns(20)` + `SetMaxIdleConns(10)` + `SetConnMaxLifetime(5*time.Minute)`
2. Add indexes: `CREATE INDEX ON ledger_entries(account_id)` + `CREATE INDEX ON ledger_entries(txn_id)`
3. Tune Postgres `max_connections` in docker-compose for v4 scale
