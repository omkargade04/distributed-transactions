# Experiment 03 — Concurrent Same-Payer Race

**Version:** v1
**Failure mode:** race condition — missing row lock
**Date:** 2026-05-24
**Duration:** ~15 minutes

---

## Hypothesis

### Q1 — How many transfers complete successfully?

**Prediction: Both succeed.**

Both goroutines read `acc_001.balance_minor = 100000` before either commits. Both pass the `balance >= amount` check (100000 >= 60000). Both proceed to UPDATE + INSERT. Both commit. No error returned to either caller.

### Q2 — What is acc_001's final balance?

**Prediction: -20000 (minor units = -$200).**

```
initial:          100000
txn_A debit:      -60000
txn_B debit:      -60000
final:            -20000
```

Started at $1000, charged $600 twice = overdrawn by $200.

### Q3 — Will verifier pass or fail? Which invariant breaks?

**Prediction: Verifier partially fails — only the app-level negative balance check.**

Crucially, I1 and I2 **still pass**:

```
I1: SUM(all ledger entries) = (-60000 + 60000) + (-60000 + 60000) = 0  ✓
I2: acc_001.balance = -20000 = initial(100000) + SUM(ledger for acc_001)(-60000 + -60000) = -20000  ✓
    balance correctly reflects both debits — no drift
I3: each txn_id has exactly 1 debit + 1 credit  ✓
```

What breaks is the **app-level business rule**: no account can go negative.

```
✓ I1 ledger sum == 0
✓ I2 no balance drift
✗     no negative balances  ← THIS fires
✓ I3 all txns have 2 entries
```

**Key insight:** ledger is mathematically consistent. We violated a business rule (can't spend money you don't have), not a structural DB invariant. The system correctly recorded two legitimate-looking transactions. The illegitimacy is only visible at the app-rule layer.

### Q4 — Why does READ COMMITTED isolation allow this?

**Because READ COMMITTED does not hold read locks.**

Precise sequence:

```
Txn A: SELECT balance WHERE id='acc_001'  → 100000  (committed value)
Txn B: SELECT balance WHERE id='acc_001'  → 100000  (same committed value)
         ↑ neither sees the other's uncommitted UPDATE

Txn A: 100000 >= 60000 → PASS → UPDATE balance = 40000 → COMMIT
Txn B: 100000 >= 60000 → PASS → UPDATE balance = 40000 - 60000 = -20000 → COMMIT
                ↑ B's balance check used the PRE-COMMIT read — the window is already closed
```

READ COMMITTED means: each statement reads the latest **committed** data at the moment that statement executes. It does NOT re-check whether the data changed between your SELECT and your UPDATE. The gap between read and write is the race window.

**Idempotency keys do NOT fix this.** Idempotency prevents duplicate requests (same intent, sent twice). This race is two *different* intents arriving simultaneously — different payees, different amounts. Legitimate concurrent load. Idempotency is irrelevant.

**DB-level fixes (v3 lesson):**

| Fix | Mechanism | Trade-off |
|-----|-----------|-----------|
| `SELECT ... FOR UPDATE` | Row lock held until COMMIT. Txn B blocks, re-reads 40000, fails balance check. | Lower throughput under contention. Simple. |
| Optimistic lock (version col) | Both read version=0. Txn A commits (version→1). Txn B's UPDATE hits version mismatch → 0 rows affected → app retries. | High throughput, requires retry logic. |
| `SERIALIZABLE` isolation | Postgres detects R-W conflict automatically, aborts one txn with `ERROR 40001`. App catches + retries. | Most correct, highest overhead. |

v3 implements `SELECT FOR UPDATE` first — simplest to understand, directly teaches the locking concept.

---

## Reproduction

### Pre-conditions
- Fresh DB state (`make reset && sleep 10`)
- Stack healthy

### Steps

```bash
# Send two concurrent transfers from acc_001 — each $600, different payees
# Run this multiple times if needed (race is timing-dependent)
for r in $(seq 1 20); do
  make reset > /dev/null 2>&1 && sleep 3

  # Fire both simultaneously
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":60000,"currency":"USD"}' &
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_003","amount_minor":60000,"currency":"USD"}' &
  wait

  # Check if race triggered
  BALANCE=$(curl -s http://localhost:8080/v1/accounts/acc_001 | python3 -c "import sys,json; print(json.load(sys.stdin)['balance_minor'])")
  if [ "$BALANCE" -lt "0" ]; then
    echo "Race triggered at attempt $r — acc_001 balance: $BALANCE"
    break
  fi
done

# Verify — expect negative balance to show
make verify
curl -s http://localhost:8080/v1/accounts/acc_001
```

---

## Observed

### HTTP responses (both 200)
```json
{"txn_id":"54c62df3-4a82-45e5-b73f-d649b8915d7e","status":"completed"}
{"txn_id":"2b663fa8-bf70-41f3-92c5-8f36cdebb7a4","status":"completed"}
```
Both completed successfully. Two different txn_ids. No error returned to either caller.

### Race triggered: attempt 1 of 20

### Account balance after
```json
{"id":"acc_001","balance_minor":-20000,"currency":"USD"}
```
-20000 = -$200. Started at $1000, charged $600 twice.

### Verifier output
```
=== Verifier Report ===
  ✓ I1 ledger sum == 0            sum=0
  ✓ I2 no balance drift           drifted=[]
  ✗     no negative balances      negative=[acc_001]
  ✓ I3 all txns have 2 entries    orphans=[]

pass: false
```
`make verify` exits 1. I1, I2, I3 all pass. Only negative balance check fires.

---

## Root cause

In `internal/ledger/transfer.go`:

```go
// Step D: read payer balance
err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID).Scan(new(string), &payerBal, new(string))

// Step F: balance check
if payerBal < req.AmountMinor {
    return nil, ErrInsufficientFunds
}

// Step G: debit
_, err = tx.ExecContext(ctx, db.QUpdateBalance, -req.AmountMinor, req.PayerID)
```

Under READ COMMITTED, the SELECT in Step D does not hold a row lock. Timeline:

```
Txn A (goroutine 1): SELECT balance → 100000   ← reads committed value
Txn B (goroutine 2): SELECT balance → 100000   ← reads same committed value
Txn A: 100000 >= 60000 → PASS
Txn B: 100000 >= 60000 → PASS
Txn A: UPDATE balance = 40000 → COMMIT
Txn B: UPDATE balance = 40000 - 60000 = -20000 → COMMIT
                             ↑ Txn B's UPDATE applies delta to the POST-commit value
                               but the balance CHECK used the PRE-commit read
```

The race window = time between Txn A's SELECT and Txn A's COMMIT. Txn B's balance check ran inside that window.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — `SUM(ledger) == 0` | ✓ holds | Two complete debit+credit pairs. (-60000+60000) + (-60000+60000) = 0. |
| I2 — balance matches ledger | ✓ holds | acc_001.balance(-20000) = initial(100000) + SUM(ledger for acc_001)(-120000) = -20000. Accurate. |
| I3 — all txn_ids have 2 rows | ✓ holds | Each txn has exactly 1 debit + 1 credit. No orphans. |
| **app — no negative balance** | ✗ **BREAKS** | acc_001 = -20000. Business rule violated. |

Ledger is mathematically consistent. Only the business rule (balance >= 0) was violated. The system faithfully recorded two transactions that should not have both been permitted.

---

## Lesson preview

**Broke because:** `transfer.go` uses `SELECT` then `UPDATE` under READ COMMITTED isolation with no row lock. Gap between read and write = race window. Two concurrent transactions can both read stale balance and both pass the check.

**Fixed in: v3** — add `SELECT balance_minor FROM accounts WHERE id = $1 FOR UPDATE` inside the transaction. Row lock held until COMMIT. Second transaction blocks, re-reads post-commit balance, correctly fails the balance check.

---

## Reflection

Race triggered on attempt 1 — far easier to reproduce than expected. READ COMMITTED is the default isolation level for Postgres and most applications. This means most production systems without explicit row locking are vulnerable to this exact race under concurrent load.

The most important observation: I1 and I2 passed. The ledger is internally consistent — it correctly recorded both debits. The system didn't malfunction from a data integrity standpoint. It just faithfully executed two instructions that should not have both been allowed. The bug is in the DECISION layer (balance check), not the EXECUTION layer (debit/credit).

Distinction burned in: READ COMMITTED protects against dirty reads (seeing another txn's uncommitted changes) but NOT against lost updates (two txns reading the same value and both writing based on it). That requires either a lock (FOR UPDATE) or conflict detection (SERIALIZABLE + retry).
