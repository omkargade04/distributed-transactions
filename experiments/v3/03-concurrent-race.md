# Experiment 03 — Concurrent Same-Payer Race (v3 rerun with FOR UPDATE)

**Version:** v3
**Failure mode:** race condition — fixed by SELECT FOR UPDATE
**Date:** 2026-05-31
**v1 baseline:** experiments/v1/03-concurrent-race.md

---

## What changed from v1

Single change in `internal/ledger/transfer.go`:

```go
// v1: no lock — race window between SELECT and UPDATE
err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID)...

// v3: row lock acquired at SELECT time, held until COMMIT
err = tx.QueryRowContext(ctx, db.QGetAccountForUpdate, req.PayerID)...
```

`QGetAccountForUpdate` = `SELECT ... FOR UPDATE` — Postgres holds an exclusive row lock on the payer's `accounts` row from this point until the transaction commits or rolls back.

---

## How FOR UPDATE closes the race

**v1 race (READ COMMITTED, no lock):**
```
Txn A: SELECT balance → 100000  (no lock)
Txn B: SELECT balance → 100000  (no lock, same committed value)
Both: 100000 >= 60000 → PASS
Txn A: UPDATE balance = 40000 → COMMIT
Txn B: UPDATE balance = 40000 - 60000 = -20000 → COMMIT  ← BUG
```

**v3 with FOR UPDATE:**
```
Txn A: SELECT balance FOR UPDATE → 100000  (acquires row lock)
Txn B: SELECT balance FOR UPDATE → BLOCKS (lock held by Txn A)

Txn A: 100000 >= 60000 → PASS → UPDATE balance = 40000 → COMMIT → lock released
Txn B: unblocks → reads 40000 (post-commit value)
Txn B: 40000 < 60000 → ErrInsufficientFunds → 400  ← CORRECT
```

---

## Reproduction

```bash
for r in $(seq 1 5); do
  # Two concurrent $600 transfers from acc_001 ($1000 balance). Only one can succeed.
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":60000}' &
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_003","amount_minor":60000}' &
  wait

  BALANCE=$(curl -s http://localhost:8080/v1/accounts/acc_001 | python3 -c "import sys,json; print(json.load(sys.stdin)['balance_minor'])")
  echo "Attempt $r: acc_001 balance = $BALANCE"
  make reset > /dev/null 2>&1 && sleep 3
done
```

---

## Observed

```
Attempt 1: 200 + 400 insufficient_funds → acc_001 balance = 40000
Attempt 2: 200 + 400 insufficient_funds → acc_001 balance = 40000
Attempt 3: 200 + 400 insufficient_funds → acc_001 balance = 40000
Attempt 4: 200 + 400 insufficient_funds → acc_001 balance = 40000
Attempt 5: 200 + 400 insufficient_funds → acc_001 balance = 40000
```

acc_001 = 40000 = 100000 - 60000. One transfer succeeded, one correctly rejected. No negative balance across all 5 attempts.

---

## v1 vs v3 comparison

| | v1 (no lock) | v3 (FOR UPDATE) |
|--|--|--|
| Race outcome | acc_001 = -20000 (both committed) | acc_001 = 40000 (one committed, one rejected) |
| Verifier | ✗ negative_balance_accts fires | ✓ all invariants pass |
| Triggered on | attempt 1 of 20 | never (0/5) |
| Second transfer response | 200 (incorrect) | 400 insufficient_funds (correct) |

---

## Invariant impact

| Invariant | v1 | v3 |
|-----------|----|----|
| I1 — ledger sum == 0 | ✓ | ✓ |
| I2 — balance matches ledger | ✓ | ✓ |
| I3 — all txns have 2 rows | ✓ | ✓ |
| app — no negative balance | ✗ broke | ✓ holds |

---

## Key observations

**`insufficient_funds` (400) is correct behavior, not an error.** The second transfer correctly detected "you can't send $600 when you only have $400." The race condition was the system failing to enforce this rule. v3 enforces it.

**Lock scope is per-row.** Concurrent transfers from `acc_001` and `acc_002` simultaneously are unaffected — they lock different rows. The serialization only applies when two transactions compete for the same payer row.

**FOR UPDATE vs SERIALIZABLE:** SERIALIZABLE would also fix this but aborts one transaction with `ERROR 40001`, requiring the app to catch and retry. FOR UPDATE is explicit and predictable — the blocked transaction waits, then re-reads, then either proceeds or fails the balance check cleanly. No retry logic needed in the app.

---

## Lesson

The race condition existed for one reason: the gap between reading a value and writing based on it, under a transaction isolation level that doesn't hold read locks. `FOR UPDATE` eliminates this gap by holding the lock from read to write. The balance check and the debit are now atomic from the perspective of other transactions competing for the same row.
