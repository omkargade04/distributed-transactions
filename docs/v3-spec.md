# v3 Specification — Distributed Payment System

**Project:** Learning Distributed Systems via Payment System
**Version:** v3 — DB optimizations (locking, pool tuning, indexes, query profiling)
**Status:** Implementation-ready
**v2 tag:** `v2` (commit e9c8196)
**Date:** 2026-05-31

---

## 0. Overview

v3 fixes the two remaining v1 failure modes via database-level changes only. No new services, no new packages, no new binaries. The lesson: before reaching for infrastructure (queues, caches, replicas), squeeze the database you already have.

**What v3 fixes:**
- Exp 03 race condition → `SELECT FOR UPDATE` on payer row
- Exp 04 pool exhaustion → `SetMaxOpenConns` + `SetMaxIdleConns` + `SetConnMaxLifetime`

**What v3 adds for learning:**
- Indexes on high-read columns
- `pg_stat_statements` for query profiling
- `EXPLAIN ANALYZE` workflow for slow query investigation

---

## 1. Decision rationale (no grill needed — obvious from v1/v2)

### SELECT FOR UPDATE vs SERIALIZABLE isolation

Both fix the race. Pick `SELECT FOR UPDATE`.

| Option | How | Tradeoff |
|--------|-----|----------|
| `SELECT FOR UPDATE` | Row lock held until COMMIT. Second txn blocks, then re-reads committed balance | Explicit, easy to reason about, slightly reduces concurrency |
| `SERIALIZABLE` | Postgres detects R-W conflict, aborts one txn with `ERROR 40001` | Automatic, but requires retry on `40001` in app code — more moving parts |

v3 lesson = locking. `SELECT FOR UPDATE` teaches the mechanism directly. Save SERIALIZABLE for v4+ when understanding the difference matters.

### Pool numbers

```go
dbx.SetMaxOpenConns(20)
dbx.SetMaxIdleConns(10)
dbx.SetConnMaxLifetime(5 * time.Minute)
```

- `MaxOpenConns=20` → Postgres sees ≤20 connections from this app. With default `max_connections=100`, leaves room for psql, verifier, other tools.
- `MaxIdleConns=10` → half of max. Keeps 10 connections warm between bursts. Avoids re-connection overhead.
- `ConnMaxLifetime=5m` → recycles connections. Prevents stale connections after Postgres restart (exp 05 root cause).

### Indexes

```sql
CREATE INDEX idx_ledger_account_id ON ledger_entries (account_id);
CREATE INDEX idx_ledger_txn_id     ON ledger_entries (txn_id);
```

Why these two:
- `account_id`: every balance check in the verifier's I2 query joins/groups by account_id. As ledger grows, full scan = O(N) vs O(log N).
- `txn_id`: I3 invariant query groups by txn_id. Also: future queries like "give me all entries for this transaction" become fast.

What we deliberately skip in v3:
- Partial indexes, composite indexes, covering indexes — v7 when load profiling reveals actual bottlenecks
- `transfers.idempotency_key` already has implicit UNIQUE index

---

## 2. Code changes

### `internal/db/queries.go` — add one new constant

```go
// QGetAccountForUpdate is identical to QGetAccount but appends FOR UPDATE,
// which acquires a row-level lock until the transaction commits or rolls back.
// Use inside a transaction to prevent concurrent balance check + update races.
const QGetAccountForUpdate = `
    SELECT id, balance_minor, currency
    FROM accounts
    WHERE id = $1
    FOR UPDATE
`
```

### `internal/ledger/transfer.go` — swap payer read to use lock

Change Step D (payer balance read):

```go
// v2: used QGetAccount (no lock)
err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID).
    Scan(new(string), &payerBal, new(string))

// v3: use QGetAccountForUpdate (acquires row lock)
err = tx.QueryRowContext(ctx, db.QGetAccountForUpdate, req.PayerID).
    Scan(new(string), &payerBal, new(string))
```

**Only the payer read needs the lock.** The payee existence check (Step E) does not — we don't read payee's balance for the check, so no lost-update risk on payee.

**What this fixes:** second concurrent transfer from same payer now BLOCKS at Step D until first commits. Then re-reads balance = 40000, fails `40000 < 60000` check → `ErrInsufficientFunds`. Race closed.

**What this costs:** concurrent transfers FROM THE SAME PAYER now serialize at DB level. Different payers are unaffected — locks are per-row. For a 100-account pool with uniform random payers, same-payer collision probability is low (~1%). Acceptable.

### `internal/db/db.go` — pool tuning

After `db.Ping()`, add:

```go
dbx.SetMaxOpenConns(20)
dbx.SetMaxIdleConns(10)
dbx.SetConnMaxLifetime(5 * time.Minute)
```

**Important:** no comment needed explaining the numbers. The fact that `MaxOpenConns=0` (unlimited) was the problem is already documented in `experiments/v1/04-pool-exhaustion.md`.

### `internal/db/migrations/0004_indexes.up.sql`

```sql
-- Indexes for ledger_entries query patterns.
-- account_id: verifier I2 (balance drift), future per-account history queries.
-- txn_id: verifier I3 (orphan check), future per-txn lookup.
CREATE INDEX idx_ledger_account_id ON ledger_entries (account_id);
CREATE INDEX idx_ledger_txn_id     ON ledger_entries (txn_id);
```

### `internal/db/migrations/0004_indexes.down.sql`

```sql
DROP INDEX IF EXISTS idx_ledger_account_id;
DROP INDEX IF EXISTS idx_ledger_txn_id;
```

### `docker-compose.yml` — enable pg_stat_statements

```yaml
postgres:
  image: postgres:16-alpine
  command: >
    postgres
    -c shared_preload_libraries=pg_stat_statements
    -c pg_stat_statements.track=all
    -c pg_stat_statements.max=1000
```

And in migrations, create the extension (add to `0001_init.up.sql` OR new migration `0005_extensions.up.sql`):

```sql
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
```

**Why docker-compose command override instead of postgresql.conf:** `shared_preload_libraries` must be set at server start. Easiest way in Docker without a custom config file = pass via command flags.

---

## 3. New `EXPLAIN ANALYZE` workflow

Standard debugging loop for any slow query:

```sql
-- Step 1: identify slow queries from pg_stat_statements
SELECT
    round(total_exec_time::numeric, 2) AS total_ms,
    calls,
    round(mean_exec_time::numeric, 2)  AS avg_ms,
    round(stddev_exec_time::numeric, 2) AS stddev_ms,
    left(query, 80) AS query_preview
FROM pg_stat_statements
ORDER BY mean_exec_time DESC
LIMIT 10;

-- Step 2: run EXPLAIN ANALYZE on the slow query
EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT)
SELECT a.id, a.balance_minor, COALESCE(SUM(l.amount_minor), 0)
FROM accounts a
LEFT JOIN ledger_entries l ON l.account_id = a.id
GROUP BY a.id, a.balance_minor
HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + 100000;

-- Step 3: look for Seq Scan on large tables → add index → re-run → confirm Index Scan
-- Step 4: reset stats between measurements
SELECT pg_stat_statements_reset();
```

**Reading EXPLAIN ANALYZE output:**

| Term | Meaning |
|------|---------|
| `Seq Scan` | Full table scan. O(N). Bad on large tables. |
| `Index Scan` | Uses index. O(log N). What you want after adding index. |
| `actual time=X..Y` | X = time to first row, Y = time to last row (ms) |
| `rows=N` | Rows processed at this node |
| `Buffers: hit=N` | Pages read from cache (fast) vs `read=N` from disk (slow) |

---

## 4. v3 Experiments (4 total)

### Exp 03 (rerun) — Race condition fixed

```bash
make reset && sleep 10

for r in $(seq 1 5); do
  # Two concurrent $600 transfers from acc_001 ($1000 balance)
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":60000,"currency":"USD"}' &
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_003","amount_minor":60000,"currency":"USD"}' &
  wait

  BALANCE=$(curl -s http://localhost:8080/v1/accounts/acc_001 | python3 -c "import sys,json; print(json.load(sys.stdin)['balance_minor'])")
  echo "Attempt $r: acc_001 balance = $BALANCE"
done

make verify
```

**Expected:** acc_001 never goes negative across all 5 attempts. One transfer succeeds ($600 deducted), the other gets `ErrInsufficientFunds` (40000 < 60000 fails after re-read). Verifier passes.

**Observation to document:** one of the two concurrent transfers gets 400 `insufficient_funds`. This is correct behavior — not an error, it's the system enforcing "you can't spend money you don't have."

### Exp 04 (rerun) — Pool exhaustion fixed

```bash
make reset && sleep 10
EXPERIMENT_ID=04 RPS=500 WORKERS=200 DURATION=30s VERSION=v3 make sim
make verify
```

**Expected:** `intents_failed` near 0 (vs 42% in v1). p99 latency increases under load (requests queue in pool rather than failing), but no SQLSTATE 53300 errors in app logs.

### Exp 08 (NEW) — Index impact on verifier query time

```bash
make reset && sleep 10
# Generate significant data: 1500 RPS × 60s = ~90,000 ledger rows
EXPERIMENT_ID=08 RPS=1500 WORKERS=200 DURATION=60s VERSION=v3 make sim

# Time verifier BEFORE indexes via pg_stat_statements
make psql
# In psql:
SELECT pg_stat_statements_reset();
\q

time make verify
# Note timing

# Now check query stats
make psql
# In psql:
SELECT round(mean_exec_time::numeric,2) AS avg_ms, calls, left(query,80) AS q
FROM pg_stat_statements ORDER BY mean_exec_time DESC LIMIT 5;
\q
```

Shows index impact at real data volume. With 90k ledger rows, full scan vs indexed scan difference becomes measurable.

### Exp 09 (NEW) — EXPLAIN ANALYZE workflow

```bash
make psql
```

Inside psql, run the full EXPLAIN ANALYZE loop:

```sql
-- 1. Baseline without index (run on fresh data before 0004 migration applied)
EXPLAIN (ANALYZE, BUFFERS)
SELECT a.id FROM accounts a
LEFT JOIN ledger_entries l ON l.account_id = a.id
GROUP BY a.id, a.balance_minor
HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + 100000;
-- Note: Seq Scan on ledger_entries

-- 2. After 0004_indexes.up.sql applied (make reset applies all migrations)
-- Re-run the same EXPLAIN ANALYZE
-- Note: Index Scan on idx_ledger_account_id

-- 3. Check top slow queries
SELECT round(mean_exec_time::numeric,2) AS avg_ms, calls, left(query,80)
FROM pg_stat_statements ORDER BY mean_exec_time DESC LIMIT 5;
```

Document the EXPLAIN output: show `Seq Scan` → `Index Scan` change. Include `actual time=` values before/after.

---

## 5. v3 done definition (7 sections, mirrors v1/v2)

1. **Code complete:**
   - `QGetAccountForUpdate` constant added
   - `transfer.go` payer read uses `FOR UPDATE`
   - `db.go` pool limits set (MaxOpenConns=20, MaxIdleConns=10, ConnMaxLifetime=5m)
   - `0004_indexes.up/down.sql` written
   - `pg_stat_statements` enabled in docker-compose + extension created

2. **Happy path:** basic transfer still works. `make sim` passes. Verifier passes. p99 still reasonable under normal load.

3. **4 experiments documented** in `experiments/v3/`

4. **Observability:** add `db.query.slow` log events (already spec'd in v1, worth actually wiring): log a `warn` event for any query exceeding 100ms in `transfer.go`.

5. **Docs:** `docs/v3-spec.md` matches built code. `experiments/v3/README.md` indexes 4 experiments. `README.md` updated with v3 notes.

6. **Migration reliability:** `make reset` applies all 5 migrations (0001-0005). `EXPLAIN (ANALYZE)` shows Index Scan on ledger_entries after migration.

7. **Reflection captured** in `experiments/v3/reflection.md`.

---

## 6. Out of scope (v3 — DO NOT add)

- Outbox pattern → v5
- Prometheus / metrics → v4
- pgbouncer / connection proxy → future
- Partial indexes, covering indexes → v7 (premature without profiling data)
- SERIALIZABLE isolation → not needed once FOR UPDATE is in
- Read replicas → v6
- Sharding → v6

---

## 7. Estimated time

```
Code:        1 hour  (tiny changes)
Experiments: 2 hours (4 × ~30 min each)
Docs:        1 hour
─────────────────────
Total:       ~4 hours = 1 evening
```
