# Experiment 01 — Duplicate Request

**Version:** v1
**Failure mode:** app bug — missing idempotency
**Date:** 2026-05-24
**Duration:** ~5 minutes

---

## Hypothesis

**Prediction: B — Both requests succeed, money debited twice.**

v1 has no idempotency logic. No `idempotency_key` field in the request, no deduplication table, no check in `ledger/transfer.go` for previously-seen requests. Every HTTP POST triggers a fresh DB transaction regardless of content. A client retrying the same payload has no way to signal "this is the same payment intent."

---

## Reproduction

### Pre-conditions
- Fresh DB state (`make reset && sleep 10`)
- Stack healthy (`curl -s http://localhost:8080/health`)

### Steps

```bash
# Transfer 1
curl -s -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Transfer 2 — identical body
curl -s -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Check balances
curl -s http://localhost:8080/v1/accounts/acc_001
curl -s http://localhost:8080/v1/accounts/acc_002

# Check invariants
make verify
```

---

## Observed

### HTTP responses
```json
{"txn_id":"f8511d7f-f541-44f5-8c7d-ef3549849352","status":"completed"}
{"txn_id":"38adfea1-727b-472b-967c-5e1065bfef7e","status":"completed"}
```
Both returned 200. Two **different** txn_ids — system treated them as separate transfers.

### Account balances after
| account | balance_minor | note |
|---------|---------------|------|
| acc_001 | 0 | was 100000 — charged 50000 twice |
| acc_002 | 200000 | was 100000 — received 50000 twice |

### Verifier output
```
=== Verifier Report ===
  ✓ I1 ledger sum == 0            sum=0
  ✓ I2 no balance drift           drifted=[]
  ✓     no negative balances      negative=[]
  ✓ I3 all txns have 2 entries    orphans=[]
```
**All invariants PASS.**

---

## Root cause

`ledger/transfer.go` opens a fresh DB transaction on every call. No `idempotency_key` in `TransferRequest`, no lookup table for previously-processed requests, no deduplication at any layer (handler, service, or DB).

Each HTTP request = independent command. When same payload sent twice, two complete ACID transactions execute — each generating a new `txn_id`, updating `accounts.balance_minor`, inserting debit+credit pair in `ledger_entries`. Both internally consistent.

Bug lives at the **application layer**, not the database layer.

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — `SUM(ledger) == 0` | ✓ holds | Each transfer inserts +50000 credit and -50000 debit. Total sum = 0. |
| I2 — balance matches ledger | ✓ holds | `acc_001.balance = 0 = 100000 + (-50000 + -50000)`. Accurate. |
| I3 — all txn_ids have 2 rows | ✓ holds | Both txn_ids have exactly 1 debit + 1 credit. No orphans. |
| app — no negative balance | ✓ holds | Balance hit 0 exactly. |

**Key insight:** Verifier passing is the whole point. ACID guarantees internal DB consistency — cannot detect business-level errors like duplicate client intent. Complete audit trail ≠ correct behavior.

---

## Lesson preview

**Broke because:** no `idempotency_key` — system has no memory of client intent. Each HTTP request = independent command.

**Fixed in: v2** — add `idempotency_key` (client-generated UUID per *intent*) to request. Service adds `transfers` table with `UNIQUE(idempotency_key)`. Duplicate key → return cached result from first execution, no second charge. Pattern: **at-most-once execution**.

---

## Reflection

Prediction correct. Most surprising: verifier passed cleanly despite acc_001 fully drained by accidental double-charge. Reveals fundamental gap — mathematical consistency of the ledger is necessary but NOT sufficient for correct payment behavior. System can be perfectly ACID-compliant and still silently double-charge customers.

Core lesson: "DB-layer correctness" (what verifier checks) ≠ "application-layer correctness" (was client intent fulfilled exactly once?). This distinction drives v1 → v2.
