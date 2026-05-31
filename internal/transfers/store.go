package transfers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Record is one row of the transfers table.
type Record struct {
	ID              uuid.UUID
	IdempotencyKey  string
	RequestHash     []byte
	RequestPayload  json.RawMessage
	ResponseStatus  *int            // null until completed
	ResponsePayload json.RawMessage // null until completed
	Status          string          // pending | completed | failed
	TxnID           *uuid.UUID
	CreatedAt       time.Time
	CompletedAt     *time.Time
	ErrorMessage    *string
}

// LookupOrReserve checks the transfers table for an existing idempotency_key.
//
// TODO (you): implement.
//
// Returns:
//   - (record, nil)             — found + completed AND request_hash matches → caller serves cached response
//   - (nil, ErrInFlight)        — found + pending → caller returns 409
//   - (nil, ErrPayloadConflict) — found AND request_hash differs → caller returns 422
//   - (nil, ErrCacheMiss)       — not found → caller should call Insert next and execute
//
// Special case: if found + status='failed' → DELETE the row and return ErrCacheMiss.
// This lets retries re-execute after a transient failure.
//
// Approach:
//   1. SELECT * FROM transfers WHERE idempotency_key = $1
//      Scan into &r fields.
//      If sql.ErrNoRows → return nil, ErrCacheMiss.
//   2. Compare bytes.Equal(r.RequestHash, incomingHash).
//      If different → return nil, ErrPayloadConflict.
//   3. Switch on r.Status:
//        case "completed": return &r, nil
//        case "pending":   return nil, ErrInFlight
//        case "failed":    DELETE row + return nil, ErrCacheMiss
//
// Pitfalls:
//   - Use bytes.Equal to compare []byte slices (NOT == which compares pointers).
//   - response_status and response_payload are nullable — Scan into *int and json.RawMessage.
//   - txn_id is nullable until completed — Scan into *uuid.UUID.
//   - completed_at + error_message are nullable strings/times — use *time.Time and *string.
func LookupOrReserve(ctx context.Context, dbx *sql.DB, key string, incomingHash []byte) (*Record, error) {
	// TODO: implement
	var r Record
	err := dbx.QueryRowContext(ctx, `
		SELECT id, idempotency_key, request_hash, request_payload,
		       response_status, response_payload, status,
		       txn_id, created_at, completed_at, error_message
		FROM transfers WHERE idempotency_key = $1
	`, key).Scan(
		&r.ID,
		&r.IdempotencyKey,
		&r.RequestHash,
		&r.RequestPayload,
		&r.ResponseStatus,
		&r.ResponsePayload,
		&r.Status,
		&r.TxnID,
		&r.CreatedAt,
		&r.CompletedAt,
		&r.ErrorMessage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCacheMiss
	}
	if err != nil {
		return nil, fmt.Errorf("lookup transfer: %w", err)
	}
	if !bytes.Equal(r.RequestHash, incomingHash) {
		return nil, ErrPayloadConflict
	}
	switch r.Status {
	case "completed":
		return &r, nil
	case "pending":
		return nil, ErrInFlight
	case "failed":
		// Clear failed rows so they can be retried fresh
		_, err = dbx.ExecContext(ctx, "DELETE FROM transfers WHERE idempotency_key = $1", key)
		if err != nil {
			return nil, fmt.Errorf("delete failed idempotency key: %w", err)
		}
		return nil, ErrCacheMiss
	default:
		return nil, fmt.Errorf("unknown transfer status: %s", r.Status)
	}
}

// Insert reserves the idempotency_key with status='pending'.
//
// TODO (you): implement.
//
// Approach:
//   INSERT INTO transfers (idempotency_key, request_hash, request_payload, status)
//   VALUES ($1, $2, $3, 'pending')
//
// On UNIQUE violation: another request reserved the same key milliseconds
// ahead of you (race between LookupOrReserve and Insert). Return ErrInFlight.
//
// Pitfalls:
//   - To detect UNIQUE violation, check the error message for SQLSTATE 23505
//     OR import pgconn and type-assert to *pgconn.PgError and check Code == "23505".
//   - Use ExecContext (we don't need the inserted row back).
func Insert(ctx context.Context, dbx *sql.DB, key string, hash []byte, payload json.RawMessage) error {
	// TODO: implement
	query := `
		INSERT INTO transfers (idempotency_key, request_hash, request_payload, status)
		VALUES ($1, $2, $3, 'pending')
	`
	_, err := dbx.ExecContext(ctx, query, key, hash, payload)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrInFlight
		}
		return fmt.Errorf("insert idempotency record: %w", err)
	}
	return nil
}

// MarkCompleted updates the row with the response and links to the ledger txn.
//
// TODO (you): implement.
//
//   UPDATE transfers
//   SET status = 'completed',
//       response_status = $1,
//       response_payload = $2,
//       txn_id = $3,
//       completed_at = now()
//   WHERE idempotency_key = $4 AND status = 'pending'
//
// The `AND status = 'pending'` guard prevents accidentally overwriting an
// already-completed row (defensive — shouldn't happen in normal flow).
func MarkCompleted(ctx context.Context, dbx *sql.DB, key string, txnID uuid.UUID, status int, body json.RawMessage) error {
	// TODO: implement
	query := `
		UPDATE transfers
		SET status = 'completed',
			response_status = $1,
			response_payload = $2,
			txn_id = $3,
			completed_at = now()
		WHERE idempotency_key = $4 AND status = 'pending'
	`
	_, err := dbx.ExecContext(ctx, query, status, body, txnID, key)
	if err != nil {
		return fmt.Errorf("mark transfer completed: %w", err)
	}
	return nil
}

// MarkFailed records that the underlying operation failed.
//
// TODO (you): implement.
//
//   UPDATE transfers
//   SET status = 'failed',
//       error_message = $1,
//       completed_at = now()
//   WHERE idempotency_key = $2 AND status = 'pending'
//
// Failed rows are not removed here — LookupOrReserve deletes them on retry.
func MarkFailed(ctx context.Context, dbx *sql.DB, key string, errMsg string) error {
	// TODO: implement
	query := `
		UPDATE transfers
		SET status = 'failed',
			error_message = $1,
			completed_at = now()
		WHERE idempotency_key = $2 AND status = 'pending'
	`
	_, err := dbx.ExecContext(ctx, query, errMsg, key)
	if err != nil {
		return fmt.Errorf("mark transfer failed: %w", err)
	}
	return nil
}

// isUniqueViolation returns true if err is a Postgres SQLSTATE 23505.
//
// Helper for Insert(). Implementation hint:
//   import "github.com/jackc/pgx/v5/pgconn"
//   var pgErr *pgconn.PgError
//   return errors.As(err, &pgErr) && pgErr.Code == "23505"
//
// Stub returns false — you wire it up.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// Fallback: pgx/v5 stdlib adapter may not preserve *pgconn.PgError
	// in the error chain when accessed via database/sql interface.
	return strings.Contains(err.Error(), "23505")
}
