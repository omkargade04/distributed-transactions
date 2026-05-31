# v2 Reflection

## What we built

Added idempotency keys (header + UNIQUE constraint + transfers table), graceful shutdown (SIGTERM drain), client retry with exponential backoff + jitter, and schema migrations via golang-migrate. No new services, no new binaries — same monolith, more resilient.

## What improved

**Retry alone cut 86% failure rate to 0.6% on app kill.** That's the most impactful single change in this version. The implementation is ~30 lines in the simulator worker loop. Production systems that have no retry on payment calls are leaving enormous reliability on the floor.

**Double-charge prevention now holds under all tested conditions.** Duplicate curl, concurrent requests, retry after crash — none of them double-charged. The UNIQUE constraint + application-level lookup provide layered protection.

## What surprised me

**replays_served stayed 0 across all retry experiments.** Expected at least some replays — requests that committed before crash, then got retried. The window for this (commit succeeds + TCP torn before response reaches client) is extremely narrow in localhost Docker. In real distributed systems with real network latency, replays would be more common. The replay path was built correctly, it just wasn't triggered under these conditions.

**The isUniqueViolation bug (exp 07) was the most educational failure.** The bug was in error detection, not business logic. The DB constraint held perfectly — acc_001 was charged once even though the app returned 500 to 4 clients. This is a concrete demonstration of defense-in-depth: the application layer failed, the database layer saved it. The fix (string fallback for SQLSTATE) exposed how driver abstractions can change error types in ways that break `errors.As` assumptions.

**p99 latency increased in retry experiments.** Exp 05 p99=351ms vs v1 p99=45ms. Retries with backoff add tail latency — worst case is 3 attempts × backoff = ~1.4s overhead. In production this would show up as latency spike during incidents. Acceptable trade-off (eventual success) but requires p99 SLAs to account for retry budgets.

**JSONB re-serializes response_payload.** Stored `{"txn_id":"...","status":"completed"}`, read back `{"status": "completed", "txn_id": "..."}`. Postgres JSONB doesn't preserve key order or whitespace. Not a bug for clients (JSON is unordered), but `Content-Length` differs between original and replay. If you need byte-for-byte replay, use TEXT. If you want queryable JSON, use JSONB and accept re-serialization.

## What was harder than expected

**The golang-migrate embed path.** `//go:embed` can't traverse `..` — migrations had to move from repo root `migrations/` to `internal/db/migrations/` to be embeddable. Also had to remove the `docker-entrypoint-initdb.d` mount from docker-compose since golang-migrate now owns schema setup. Discovering this during scaffold (not during coding) saved time.

**Graceful shutdown timing.** `stop_grace_period: 35s` in docker-compose must be strictly greater than `srv.Shutdown(30s)` context timeout. Getting the ordering wrong means Docker SIGKILLs the app before the drain finishes. Easy to get right once you understand the relationship.

## Core lessons from v2

1. **Retry + idempotency = the pair.** Retry without idempotency = double charges. Idempotency without retry = clients don't know when to try again. They're inseparable for reliable payment systems.

2. **Canonical JSON hashing is non-trivial.** `{"amount":1500,"payer":"acc_001"}` and `{"payer":"acc_001","amount":1500}` must hash identically (same intent, different key order). Requires a round-trip through `json.Unmarshal` to normalize struct → map, then sorted-key serialization. Direct `json.Marshal` of Go structs doesn't sort keys.

3. **UNIQUE constraint is the atomic gate.** Between `LookupOrReserve` (no row found) and `Insert` (row created), another request can slip in. The UNIQUE violation on the INSERT is the actual atomic gate — not the lookup. The lookup is a fast-path optimization. Critical invariants belong in the DB, not just the app.

4. **Error type assumptions break at driver boundaries.** `errors.As(err, &pgconn.PgError{})` works in native pgx mode. When you go through `database/sql` stdlib, the error chain may differ. Always test error detection with the actual interface layer you deploy with.

5. **Graceful shutdown is asymmetric.** Protects against `SIGTERM` (normal `docker stop`, Kubernetes rolling deploy). Does NOT protect against `SIGKILL` (`docker kill`, OOM, kernel kill). v2 improved the common case — crashes and OOMs remain v8's problem.

## What v3 needs to fix

From exp 03 (still unfixed):
- `SELECT FOR UPDATE` on payer row in `ledger/transfer.go`
- Concurrent same-payer transfers still create negative balance

From exp 04 (still unfixed):
- `dbx.SetMaxOpenConns(20)` + `SetMaxIdleConns(10)` + `SetConnMaxLifetime`
- No indexes on `ledger_entries.account_id` or `ledger_entries.txn_id`

Also v3:
- `pg_stat_statements` enablement for query profiling
- `EXPLAIN ANALYZE` workflow for slow query identification
