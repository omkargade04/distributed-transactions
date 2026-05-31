# v3 Reflection

## What we built

Three lines of Go + one SQL keyword. The entire v3 lesson is:

```go
// db.go
db.SetMaxOpenConns(20)
db.SetMaxIdleConns(10)
db.SetConnMaxLifetime(5 * time.Minute)
```

```sql
-- queries.go
SELECT id, balance_minor, currency FROM accounts WHERE id = $1 FOR UPDATE
```

Plus two migrations (indexes + pg_stat_statements). No new packages. No new middleware. Smallest code volume of any version, but closed two of the original five failure modes.

## What surprised me

**FOR UPDATE introduced a new failure mode (deadlock) while fixing the race.** Didn't predict this. When acc_001→acc_002 and acc_002→acc_001 run simultaneously, FOR UPDATE creates a lock cycle. Postgres detects and aborts one. The retry succeeds. Net effect: the system is more correct (no negative balances) but has a new class of transient failure (deadlock → 500 → retry). This is the classic locking trade-off: serialization eliminates race conditions but introduces contention and deadlock risk.

The fix for deadlock (lock in consistent order — always lock lower account_id first) is a 3-line change, but it demonstrates that locking is not a free fix. Every lock adds contention; the design of which locks to take and in what order matters.

**p99 inversion is dramatic.** v1 p99=17ms looked "fast" — because instant failures are fast. v3 p99=771ms looks "slow" — because requests are queuing. But v3 is strictly better: 14696/14718 intents succeeded vs 8611/14809. The p99 metric alone is misleading without pairing it with success rate. Always read latency and success rate together.

**Two NULL scan bugs in the same codebase.** Both in `LookupOrReserve`: `json.RawMessage` won't accept NULL from `database/sql` (unlike plain `[]byte`), and `*int` needs `sql.NullInt32` for nullable integers. These bugs were invisible until v3's high-RPS test created enough concurrency to trigger pending-row lookups. The lesson: test your DB scan code against NULL values explicitly — they only appear at runtime under specific conditions.

**The effort estimate was accurate.** ~4 hours including experiments + docs. v3 really is DB configuration and one SQL keyword. The lesson from this: most early-stage performance problems can be fixed at the DB layer before reaching for infrastructure. This version fixed 2 of 5 original failure modes with trivial code.

## Core lessons from v3

1. **SELECT FOR UPDATE is the simplest correct locking primitive.** It's pessimistic (blocks), explicit, and predictable. The alternative (SERIALIZABLE) is automatic but requires app-level retry on `40001`. FOR UPDATE is better for learning because you can reason about exactly what's locked.

2. **Row locks are per-row, not per-table.** acc_001→acc_003 and acc_002→acc_004 execute fully in parallel — different payer rows, no contention. Only same-payer concurrent transfers serialize. This is why FOR UPDATE is practical even at high load.

3. **Deadlock = circular lock dependency.** A→B locks A, then B. B→A locks B, then A. Both wait for each other. Prevention: canonical lock order (always acquire in ID order). Detection: Postgres automatically detects and aborts one.

4. **Connection pool is client-side backpressure.** `MaxOpenConns` is the knob between "fail fast with SQLSTATE 53300" and "queue and eventually succeed." Setting it requires knowing your Postgres `max_connections` and how many other clients exist. 20 is conservative; real sizing = `(max_connections - overhead) / num_app_instances`.

5. **NULL scanning is a landmine in Go's database/sql.** `database/sql` handles `*string`, `*int`, `*time.Time` for nullables correctly. Named types (`json.RawMessage`, custom `type ID = string`) may not work. When in doubt: scan into a primitive then assign.

## What v4 needs

- Prometheus + Grafana full observability stack (original v4 plan)
- Lock ordering fix for deadlocks (`ORDER BY id` before acquiring FOR UPDATE locks)
- Worker queue (DB-backed Outbox pattern, first consumer)
- pg_stat_statements workflow — deliberately skipped in v3 experiments for speed. Worth adding as exp 08/09 before tagging v4.
