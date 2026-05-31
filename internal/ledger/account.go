package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/omkargade/distributed-payment-system/internal/db"
)

type Account struct {
	ID           string `json:"id"`
	BalanceMinor int64  `json:"balance_minor"`
	Currency     string `json:"currency"`
}

func GetAccount(ctx context.Context, dbx *sql.DB, id string) (*Account, error) {
	var a Account
	err := dbx.QueryRowContext(ctx, db.QGetAccount, id).Scan(&a.ID, &a.BalanceMinor, &a.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	return &a, nil
}
