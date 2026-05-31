# Experiment 06 — Idempotency Key Conflict (Same Key, Different Payload)

**Version:** v2
**Failure mode:** app bug prevention — client sends different payload with same key
**Date:** 2026-05-31
**New in v2** — no v1 equivalent

---

## Hypothesis

**Prediction: Second call returns 422. Only first payload executes.**

The `transfers` table stores `request_hash = SHA256(canonical JSON)` when the first request executes. When the second request arrives with the same `Idempotency-Key` but different `amount_minor`, `HashCanonical` produces a different hash. `LookupOrReserve` does `bytes.Equal(stored_hash, incoming_hash)` → false → returns `ErrPayloadConflict` → middleware returns 422 Unprocessable Entity.

Without this check, a client retry with accidentally-changed payload would silently execute a second, different payment. 422 makes the mistake loud and stops it.

---

## Reproduction

```bash
make reset && sleep 10
KEY=$(uuidgen | tr 'A-Z' 'a-z')

# First — executes ($500)
curl -si -X POST http://localhost:8080/v1/transfer \
  -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Same key, DIFFERENT amount ($999.99) — should be rejected
curl -si -X POST http://localhost:8080/v1/transfer \
  -H "Idempotency-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":99999,"currency":"USD"}'

curl -s http://localhost:8080/v1/accounts/acc_001
make verify
```

---

## Observed

```
KEY=ee963f59-2109-4c62-aa37-76695d44c379

Call 1: HTTP/1.1 200 OK
        {"txn_id":"9f02d4f3-2ea8-45c8-ab9c-8a43dec3ffbd","status":"completed"}

Call 2: HTTP/1.1 422 Unprocessable Entity
        {"error":"idempotency_key_conflict"}

acc_001 balance: {"id":"acc_001","balance_minor":50000,"currency":"USD"}
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

## Root cause (intentional design)

When Call 1 executed:
1. `HashCanonical({"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"})` → `hash_A`
2. `transfers.Insert(key, hash_A, payload, "pending")` → row created
3. `ledger.Transfer()` succeeds → `transfers.MarkCompleted(key, txn_id, 200, body)`

When Call 2 arrived:
1. `HashCanonical({"payer_id":"acc_001","payee_id":"acc_002","amount_minor":99999,"currency":"USD"})` → `hash_B`
2. `transfers.LookupOrReserve(key, hash_B)`:
   - Found row with `hash_A`
   - `bytes.Equal(hash_A, hash_B)` = **false**
   - Returns `ErrPayloadConflict`
3. Middleware returns `422 idempotency_key_conflict`
4. **No ledger write. No money moved.**

---

## Invariant impact

| Invariant | Status | Why |
|-----------|--------|-----|
| I1 — ledger sum == 0 | ✓ holds | Only 2 ledger rows (1 debit + 1 credit from Call 1). |
| I2 — balance drift | ✓ holds | acc_001 = 50000, ledger sum for acc_001 = -50000. Match. |
| I3 — 2 rows per txn | ✓ holds | One txn_id, two rows. |
| **business rule** | ✓ protected | $999.99 charge never executed. |

---

## Why 422 not 409

| Code | Meaning | When |
|------|---------|------|
| 409 Conflict | First call still running | Transient — retry will resolve |
| 422 Unprocessable Entity | Caller sent inconsistent data | Permanent — retry won't help |

422 signals a **caller bug**: your retry changed the payload. Fix your client code. Do not retry.

---

## What this protects against

```
Scenario: client sends $500 payment, gets connection error, decides to retry.
Bug: retry code accidentally reads a different amount from UI state → sends $999.99.

Without conflict check:
  Key=X, $500 → executes → stored as completed
  Key=X, $999.99 → LookupOrReserve finds key → hash differs → EXECUTES FRESH → double charge

With conflict check:
  Key=X, $500 → executes
  Key=X, $999.99 → 422 idempotency_key_conflict → client told it made a mistake
```

The canonical JSON hash means even whitespace/key-order differences don't trigger false positives: `{"amount":1500,"payer":"acc_001"}` and `{"payer":"acc_001","amount":1500}` produce the same hash.

---

## Lesson

Idempotency is not just "don't execute twice." It also enforces **intent consistency**: the same key must always represent the same intent. Any deviation is a client bug that should fail loudly.

This pairs with the replay behavior (exp 01): same key + same payload = replay. Same key + different payload = 422. The transfers table is the single source of truth for what a given key means.
