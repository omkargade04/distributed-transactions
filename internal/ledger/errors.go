package ledger

import "errors"

// Domain errors. Compare with errors.Is in callers.
//
// Why a package-level var instead of a new struct per call?
//   - Stable identity: caller can do errors.Is(err, ErrInsufficientFunds)
//   - Zero allocation per error case
//   - Easy to map to HTTP status codes in the handler layer

var (
	ErrAccountNotFound   = errors.New("account_not_found")
	ErrInsufficientFunds = errors.New("insufficient_funds")
	ErrSamePayerPayee    = errors.New("same_payer_payee")
	ErrInvalidAmount     = errors.New("invalid_amount")
)
