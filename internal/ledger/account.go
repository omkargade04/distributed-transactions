package ledger

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/omkargade/distributed-payment-system/internal/db"
)

// Account mirrors the accounts table row shape.
type Account struct {
	ID           string
	BalanceMinor int64
	Currency     string
}

// GetAccount returns the account by id, or ErrAccountNotFound if missing.
//
// TODO (you): implement this function.
//
// Requirements:
//   1. Run db.QGetAccount with QueryRowContext, scan into &a fields.
//   2. If sql.ErrNoRows → return nil, ErrAccountNotFound (from errors.go).
//   3. Any other error → wrap with fmt.Errorf("get account: %w", err).
//   4. Else return &a, nil.
//
// Hint — Go idioms:
//   - errors.Is(err, sql.ErrNoRows) is the right comparison (not == ).
//   - QueryRowContext signature: QueryRowContext(ctx, query, args...).Scan(dests...)
//   - Pass &a.ID, &a.BalanceMinor, &a.Currency in scan — pointers so Scan can write to them.
func GetAccount(ctx context.Context, dbx *sql.DB, id string) (*Account, error) {
	// TODO: implement
	_ = db.QGetAccount // remove this line once you reference it in your implementation
	return nil, fmt.Errorf("GetAccount not implemented")
}
