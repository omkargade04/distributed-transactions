package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/omkargade/distributed-payment-system/internal/db"
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
	TxnID  string `json:"txn_id"`
	Status string `json:"status"`
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
func Transfer(ctx context.Context, dbx *sql.DB, req TransferRequest) (*TransferResult, error) {
	start := time.Now()

	// A. Validate input — return sentinel errors so the handler layer can
	//    errors.Is them to specific HTTP status codes.
	if req.AmountMinor <= 0 {
		return nil, ErrInvalidAmount
	}
	if req.PayerID == req.PayeeID {
		return nil, ErrSamePayerPayee
	}

	// B. Generate new txn_id (groups the debit + credit ledger pair).
	txnID := uuid.New()

	// C. Begin transaction. nil = default isolation (READ COMMITTED).
	//    READ COMMITTED is the source of failure mode #3 (race) — fixed in v3.
	tx, err := dbx.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // safe even after Commit — Postgres ignores it.

	// D. Read payer balance INSIDE the transaction (tx, not dbx).
	var payerBal int64
	err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID).
		Scan(new(string), &payerBal, new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read payer: %w", err)
	}

	// E. Read payee — just existence check. Discard values via throwaway pointers.
	err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayeeID).
		Scan(new(string), new(int64), new(string))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read payee: %w", err)
	}

	// F. Balance check.
	//    NOTE: this is the v1 race-condition lab. Two concurrent transfers
	//    from the same payer can BOTH pass this check before either commits
	//    (under READ COMMITTED + no row lock). Documented in experiment 03.
	if payerBal < req.AmountMinor {
		return nil, ErrInsufficientFunds
	}

	slog.DebugContext(ctx, "transfer.validated",
		"txn_id", txnID,
		"payer_balance", payerBal,
	)

	// G. Debit payer (negative delta).
	if _, err = tx.ExecContext(ctx, db.QUpdateBalance, -req.AmountMinor, req.PayerID); err != nil {
		return nil, fmt.Errorf("debit payer: %w", err)
	}

	// H. Credit payee (positive delta).
	if _, err = tx.ExecContext(ctx, db.QUpdateBalance, req.AmountMinor, req.PayeeID); err != nil {
		return nil, fmt.Errorf("credit payee: %w", err)
	}

	// I. Insert debit ledger entry.
	if _, err = tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayerID, -req.AmountMinor); err != nil {
		return nil, fmt.Errorf("ledger debit: %w", err)
	}

	// J. Insert credit ledger entry (same txn_id groups the pair).
	if _, err = tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayeeID, req.AmountMinor); err != nil {
		return nil, fmt.Errorf("ledger credit: %w", err)
	}

	// K. Commit. After this, balance + ledger are durable.
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	slog.InfoContext(ctx, "transfer.completed",
		"txn_id", txnID,
		"payer_id", req.PayerID,
		"payee_id", req.PayeeID,
		"amount_minor", req.AmountMinor,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	return &TransferResult{
		TxnID:  txnID.String(),
		Status: "completed",
	}, nil
}
