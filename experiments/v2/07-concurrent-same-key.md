# Experiment 07 — Concurrent Same-Key Requests

**Version:** v2
**Failure mode:** race condition in idempotency layer
**Date:** 2026-05-31
**New in v2** — no v1 equivalent

---

## Hypothesis

**Prediction:** 5 concurrent POSTs with same Idempotency-Key — only 1 executes. Others return 409 `request_in_progress`.

All 5 arrive simultaneously, all hit `LookupOrReserve` before any INSERT happens, all see `ErrCacheMiss`. All 5 then race to `INSERT INTO transfers (idempotency_key, ...)`. One wins (UNIQUE constraint). Others get SQLSTATE 23505 → `isUniqueViolation()` → `ErrInFlight` → 409 to client.

---

## Reproduction

```bash
make reset && sleep 10
KEY=$(uuidgen | tr 'A-Z' 'a-z')

for i in 1 2 3 4 5; do
  curl -s -w " HTTP:%{http_code}\n" -X POST http://localhost:8080/v1/transfer \
    -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}' &
done
wait

curl -s http://localhost:8080/v1/accounts/acc_001
make verify
```

---

## Observed

```
KEY=566f080b-cd4f-4e5d-8e76-93c9df45060b

{"txn_id":"7510377d-b9f8-44ca-8293-d0ab356f0fd3","status":"completed"} HTTP:200
{"error":"internal"} HTTP:500
{"error":"internal"} HTTP:500
{"error":"internal"} HTTP:500
{"error":"internal"} HTTP:500

acc_001: {"id":"acc_001","balance_minor":50000,"currency":"USD"}
```

Verifier:
```
  ✓ I1 ledger sum == 0
  ✓ I2 no balance drift
  ✓     no negative balances
  ✓ I3 all txns have 2 entries
  pass: true
```

---

## Analysis

### What was expected vs what happened

| Response | Expected | Actual |
|----------|---------|--------|
| 1 request | 200 OK | 200 OK ✓ |
| 4 requests | **409** request_in_progress | **500** internal ✗ |
| acc_001 balance | 50000 (charged once) | 50000 ✓ |
| Verifier | pass | pass ✓ |

**Core idempotency held — acc_001 charged ONCE.** The UNIQUE constraint on `idempotency_key` prevented double execution at the DB level. But the error signal back to clients was wrong.

### Root cause: `isUniqueViolation` bug

```go
// store.go
func isUniqueViolation(err error) bool {
    var pgErr *pgconn.PgError
    return errors.As(err, &pgErr) && pgErr.Code == "23505"  // ← returns false
}
```

When using pgx v5 through the `database/sql` stdlib adapter, Postgres UNIQUE violation errors are wrapped differently in the error chain than native pgx errors. `errors.As(err, &pgErr)` fails to unwrap → returns `false`.

Cascade:
```
INSERT fails with SQLSTATE 23505
→ isUniqueViolation() = false
→ Insert() returns raw DB error (not ErrInFlight)
→ idempotency middleware: err != ErrInFlight → writeJSON(w, 500, "internal")
```

Clients got 500 instead of 409. In production, a client receiving 500 would retry (500 is retryable). Eventually the original request completes → they'd get a replay (200 + Idempotency-Replay: true). So the system would self-correct — but with more retries and confusion than necessary.

### Fix needed

```go
func isUniqueViolation(err error) bool {
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        return pgErr.Code == "23505"
    }
    // Fallback: pgx/v5 stdlib adapter may not preserve *pgconn.PgError in chain
    return strings.Contains(err.Error(), "23505")
}
```

With this fix, concurrent duplicates return 409 instead of 500 — cleaner signal, no behavior change at DB level.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — ledger sum == 0 | ✓ holds | Only 1 debit + 1 credit pair. |
| I2 — balance drift | ✓ holds | acc_001 = 50000 = initial(100000) + ledger(-50000). |
| I3 — 2 rows per txn | ✓ holds | One txn_id, two rows. |
| app — no negative balance | ✓ holds | |
| **client signal** | ✗ wrong | 4 clients got 500 (retryable) instead of 409 (wait, then replay) |
| **double charge** | ✓ prevented | UNIQUE constraint blocked at DB level regardless of app bug |

---

## Key insight: two layers of protection

The experiment exposes an important lesson about layered defense:

**Layer 1 (app):** `isUniqueViolation` detects UNIQUE violation → returns ErrInFlight → 409.
**Layer 2 (DB):** `UNIQUE(idempotency_key)` constraint prevents double INSERT regardless of app behavior.

Layer 1 was broken (bug in error detection). Layer 2 held perfectly. No double charge.

This is why DB constraints matter even when the application is supposed to enforce them: the DB is the last line of defense.

---

## Lesson

Two lessons from exp 07:

1. **DB constraints are the safety net.** Even with a bug in the application-level UNIQUE violation detection, the database constraint prevented the double charge. Always enforce critical invariants at the DB layer, not just in application code.

2. **Error wrapping is subtle in Go.** `errors.As` traverses the error chain, but the chain depends on how the library wraps errors. When mixing pgx native mode and `database/sql` stdlib adapter, error types may differ. Always test with the actual driver you deploy with, not an assumed interface.

---

## Fix (to apply before v2 is tagged)

In `internal/transfers/store.go`, replace `isUniqueViolation`:

```go
import "strings"

func isUniqueViolation(err error) bool {
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        return pgErr.Code == "23505"
    }
    return strings.Contains(err.Error(), "23505")
}
```
