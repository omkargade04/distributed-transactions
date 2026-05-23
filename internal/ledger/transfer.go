package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// TransferRequest is the input to Transfer().
type TransferRequest struct {
	PayerID     string
	PayeeID     string
	AmountMinor int64
	Currency    string
}

// TransferResult is what we return on success.
type TransferResult struct {
	TxnID  string
	Status string
}

// Transfer moves AmountMinor from PayerID to PayeeID atomically.
//
// THE V1 ACID LESSON LIVES HERE. Five SQL statements must all succeed (or all fail) in one
// database transaction:
//   1. Read payer balance
//   2. Read payee (existence check)
//   3. UPDATE accounts to debit payer
//   4. UPDATE accounts to credit payee
//   5. INSERT 2 ledger_entries (debit row + credit row, same txn_id)
//
// If any step errors, the entire transaction must roll back. We use defer tx.Rollback() —
// Postgres ignores rollback after a successful commit, so this is safe.
//
// TODO (you): implement this function.
//
// Step-by-step requirements:
//
//   A. Validate input (no DB call yet):
//      - if AmountMinor <= 0 → return ErrInvalidAmount
//      - if PayerID == PayeeID → return ErrSamePayerPayee
//
//   B. Generate a new txn_id: uuid.New() returns a uuid.UUID
//
//   C. Begin a DB transaction:
//      - dbx.BeginTx(ctx, nil) returns (*sql.Tx, error)
//      - nil = default isolation level (READ COMMITTED in Postgres)
//      - DELIBERATELY default — race condition surfaces in failure mode #3 → v3 lesson
//      - Use `defer tx.Rollback()` immediately after — safe even after Commit
//
//   D. Read payer balance:
//      - Use db.QGetAccount via tx.QueryRowContext (NOT dbx!) so it's inside the txn
//      - Scan into a int64 variable for balance. (For other fields you don't need, scan into new(string) — a throwaway pointer.)
//      - errors.Is(err, sql.ErrNoRows) → return ErrAccountNotFound
//      - Other err → wrap and return
//
//   E. Read payee (just existence — discard returned values):
//      - Same pattern as D. errors.Is sql.ErrNoRows → ErrAccountNotFound.
//
//   F. Balance check:
//      - if payerBalance < req.AmountMinor → return ErrInsufficientFunds
//
//   G. Debit payer:
//      - tx.ExecContext(ctx, db.QUpdateBalance, -req.AmountMinor, req.PayerID)
//      - Note the minus sign — balance decreases.
//
//   H. Credit payee:
//      - tx.ExecContext(ctx, db.QUpdateBalance, req.AmountMinor, req.PayeeID)
//
//   I. Insert debit ledger entry:
//      - tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayerID, -req.AmountMinor)
//
//   J. Insert credit ledger entry:
//      - tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayeeID, req.AmountMinor)
//
//   K. Commit:
//      - if err := tx.Commit(); err != nil → wrap and return
//
//   L. Return success:
//      - return &TransferResult{TxnID: txnID.String(), Status: "completed"}, nil
//
// LOGGING (do this as you implement):
//   - After balance check passes, slog.DebugContext(ctx, "transfer.validated", "txn_id", txnID, "payer_balance", payerBal)
//   - After commit succeeds, slog.InfoContext(ctx, "transfer.completed", ...with txn_id, payer_id, payee_id, amount_minor, duration_ms...)
//   - duration_ms = time.Since(start).Milliseconds() where start = time.Now() at function entry
//
// COMMON PITFALLS:
//   - Using `dbx` instead of `tx` for queries → ESCAPES the transaction. Atomicity broken.
//   - Forgetting `defer tx.Rollback()` → connection leaks on early return.
//   - Using sql.Open("postgres", ...) instead of "pgx" → driver not registered. We use pgx (see db.go).
//   - Comparing errors with == instead of errors.Is — wrapped errors won't match.
//
// Pseudo-flow:
//
//     start := time.Now()
//     // validations...
//     txnID := uuid.New()
//     tx, err := dbx.BeginTx(ctx, nil)
//     if err != nil { return nil, fmt.Errorf("begin: %w", err) }
//     defer tx.Rollback()
//
//     var payerBal int64
//     err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID).Scan(new(string), &payerBal, new(string))
//     // ... etc
func Transfer(ctx context.Context, dbx *sql.DB, req TransferRequest) (*TransferResult, error) {
	// TODO: implement
	_ = time.Now()
	_ = uuid.New()
	return nil, fmt.Errorf("Transfer not implemented")
}
