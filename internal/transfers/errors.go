package transfers

import "errors"

// Sentinel errors for the idempotency layer.
//
// Callers compare with errors.Is(err, transfers.ErrXxx).

var (
	// ErrCacheMiss — no row with this idempotency_key. Caller should Insert + execute fresh.
	ErrCacheMiss = errors.New("idempotency_cache_miss")

	// ErrInFlight — row exists with status=pending. Caller should return 409 to client.
	ErrInFlight = errors.New("idempotency_in_flight")

	// ErrPayloadConflict — same key, different request_hash. Caller should return 422.
	ErrPayloadConflict = errors.New("idempotency_payload_conflict")
)
