# v2 Specification — Distributed Payment System

**Project:** Learning Distributed Systems via Payment System
**Version:** v2 — idempotency + graceful shutdown
**Status:** Implementation-ready
**Source decisions:** `V2-GRILL-DECISIONS.md`
**Date:** 2026-05-31
**v1 tag:** `v1` (commit 826fed7)

---

## 0. Overview

v2 fixes three v1 failure modes by adding **idempotency keys** + **graceful shutdown** + **client retry with backoff**. The system stops silently double-charging, drains cleanly on SIGTERM, and survives transient Postgres outages without dropping payment intents.

What v2 deliberately does NOT fix: race condition (exp 03 → v3), pool exhaustion (exp 04 → v3), event emission (outbox → v5).

---

## 1. Architecture delta

```mermaid
graph TD
    Sim[Simulator<br/>w/ retry + backoff] -->|POST + Idempotency-Key| API[payment-api]
    API -->|check key| TBL[(transfers table<br/>NEW)]
    API -->|on miss: execute| LDGR[(ledger_entries)]
    API -->|on hit: replay| TBL
    Verifier[Verifier] -.->|read| TBL
    Verifier -.->|read| LDGR
    API -->|graceful drain on SIGTERM| Sig[SIGTERM handler<br/>NEW]
```

**Request flow with idempotency:**

```
1. Client → POST /v1/transfer  + header Idempotency-Key: <uuid>
2. Server middleware:
   a. SELECT * FROM transfers WHERE idempotency_key = $1
   b. IF found AND status=completed AND request_hash matches:
        → return cached response_status + response_payload
        → add header Idempotency-Replay: true
   c. IF found AND status=pending:
        → return 409 + Retry-After: 1
   d. IF found AND request_hash differs:
        → return 422 idempotency_key_conflict
   e. IF found AND status=failed:
        → DELETE row, fall through to fresh execution
   f. IF not found:
        → INSERT (idempotency_key, request_hash, request_payload, status=pending)
        → execute ledger.Transfer()
        → UPDATE transfers SET response_*, status=completed/failed, txn_id, completed_at
        → return response
```

---

## 2. Repository changes

### New files
```
internal/transfers/
├── store.go         # transfers table CRUD
├── hash.go          # canonical JSON + SHA-256
└── errors.go        # ErrIdempotencyConflict, ErrInFlight, ErrCacheMiss

internal/api/
└── idempotency.go   # middleware that consults transfers table

internal/db/
└── migrate.go       # golang-migrate runner

experiments/v2/
├── README.md
├── 01-duplicate-request.md
├── 02-process-kill.md
├── 05-postgres-restart.md
├── 06-payload-conflict.md   # NEW
├── 07-concurrent-same-key.md # NEW
└── reflection.md

migrations/
├── 0001_init.up.sql / .down.sql
├── 0002_seed.up.sql / .down.sql
├── 0003_transfers.up.sql / .down.sql
```

### Modified files
```
cmd/payment-api/main.go     # signal.Notify + srv.Shutdown
cmd/simulator/main.go       # retry loop + per-attempt records + new counters
internal/api/handler.go     # invoke idempotency middleware before ledger.Transfer
internal/api/server.go      # register idempotency middleware
internal/ledger/transfer.go # accept txn_id from caller (no longer self-generated)
docker-compose.yml          # stop_grace_period: 35s
Makefile                    # optionally: migrate-up, migrate-down targets
go.mod                      # add github.com/golang-migrate/migrate/v4
README.md                   # v2 quickstart + idempotency curl example
```

### Unchanged
- `internal/config/`, `internal/db/db.go`, `internal/ledger/account.go`
- `cmd/verifier/main.go` — verifier doesn't need to know about transfers table (it checks ledger invariants which are unchanged)

---

## 3. Schema migrations

### `migrations/0001_init.up.sql`
Same as v1 schema (accounts + ledger_entries).

### `migrations/0001_init.down.sql`
```sql
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS accounts;
DROP EXTENSION IF EXISTS pgcrypto;
```

### `migrations/0002_seed.up.sql`
Same as v1 seed (idempotent INSERT).

### `migrations/0002_seed.down.sql`
```sql
DELETE FROM accounts WHERE id LIKE 'acc_%';
```

### `migrations/0003_transfers.up.sql`
```sql
CREATE TABLE IF NOT EXISTS transfers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key   TEXT NOT NULL UNIQUE,
    request_hash      BYTEA NOT NULL,
    request_payload   JSONB NOT NULL,
    response_status   INT,
    response_payload  JSONB,
    status            TEXT NOT NULL DEFAULT 'pending',
    txn_id            UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error_message     TEXT,
    CHECK (status IN ('pending', 'completed', 'failed'))
);

CREATE INDEX idx_transfers_cleanup ON transfers (created_at)
    WHERE status != 'pending';
```

### `migrations/0003_transfers.down.sql`
```sql
DROP INDEX IF EXISTS idx_transfers_cleanup;
DROP TABLE IF EXISTS transfers;
```

---

## 4. Module: `internal/db/migrate.go`

```go
package db

import (
    "embed"
    "fmt"

    "github.com/golang-migrate/migrate/v4"
    "github.com/golang-migrate/migrate/v4/database/postgres"
    "github.com/golang-migrate/migrate/v4/source/iofs"
    _ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS  // OR: read from disk at ../migrations/

// Migrate applies all pending migrations to bring DB to latest schema.
// Idempotent — running twice does nothing the second time.
//
// Uses schema_migrations table (managed by golang-migrate) to track applied versions.
func Migrate(dbURL string) error {
    src, err := iofs.New(migrationsFS, "migrations")
    if err != nil {
        return fmt.Errorf("migrate.iofs: %w", err)
    }

    m, err := migrate.NewWithSourceInstance("iofs", src, dbURL)
    if err != nil {
        return fmt.Errorf("migrate.new: %w", err)
    }
    defer m.Close()

    err = m.Up()
    if err != nil && err != migrate.ErrNoChange {
        return fmt.Errorf("migrate.up: %w", err)
    }
    return nil
}
```

**Decision on migration source:**
- Option A: `//go:embed migrations/*.sql` — migrations baked into binary, no external file dependency at runtime
- Option B: read from `./migrations/` at runtime — requires migrations dir to be mounted in container

Recommend **Option A (embed)** — simpler ops, single binary deploys cleanly.

But! Docker `docker-entrypoint-initdb.d` still wants raw SQL files. So migrations live at `migrations/` AND get embedded into the Go binary. Postgres `entrypoint-initdb.d` mount **is removed** — golang-migrate handles all schema setup now.

### `docker-compose.yml` change for this:
```yaml
postgres:
  volumes:
    - pgdata:/var/lib/postgresql/data
    # REMOVED: - ./migrations:/docker-entrypoint-initdb.d:ro
```

Migration responsibility moves from "Postgres on first init" → "app on every startup."

---

## 5. Module: `internal/transfers/hash.go`

```go
package transfers

import (
    "bytes"
    "crypto/sha256"
    "encoding/json"
    "fmt"
    "sort"
)

// HashCanonical computes SHA-256 of the canonical JSON encoding of v.
//
// Canonical = sorted object keys, no whitespace. Identical semantic content
// produces identical hash regardless of input formatting.
//
// Used to detect "same idempotency_key with different payload" — caller bug.
func HashCanonical(v any) ([]byte, error) {
    buf := &bytes.Buffer{}
    enc := json.NewEncoder(buf)
    if err := enc.Encode(v); err != nil {
        return nil, fmt.Errorf("encode: %w", err)
    }

    // re-decode + re-encode with sorted keys for canonical form
    var generic any
    if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
        return nil, fmt.Errorf("decode: %w", err)
    }

    canonical, err := canonicalMarshal(generic)
    if err != nil {
        return nil, fmt.Errorf("canonical marshal: %w", err)
    }

    h := sha256.Sum256(canonical)
    return h[:], nil
}

// canonicalMarshal serializes with map keys sorted at every nesting level.
func canonicalMarshal(v any) ([]byte, error) {
    switch t := v.(type) {
    case map[string]any:
        keys := make([]string, 0, len(t))
        for k := range t {
            keys = append(keys, k)
        }
        sort.Strings(keys)

        buf := &bytes.Buffer{}
        buf.WriteByte('{')
        for i, k := range keys {
            if i > 0 {
                buf.WriteByte(',')
            }
            kb, _ := json.Marshal(k)
            buf.Write(kb)
            buf.WriteByte(':')
            vb, err := canonicalMarshal(t[k])
            if err != nil {
                return nil, err
            }
            buf.Write(vb)
        }
        buf.WriteByte('}')
        return buf.Bytes(), nil
    case []any:
        buf := &bytes.Buffer{}
        buf.WriteByte('[')
        for i, item := range t {
            if i > 0 {
                buf.WriteByte(',')
            }
            ib, err := canonicalMarshal(item)
            if err != nil {
                return nil, err
            }
            buf.Write(ib)
        }
        buf.WriteByte(']')
        return buf.Bytes(), nil
    default:
        return json.Marshal(v)
    }
}
```

---

## 6. Module: `internal/transfers/errors.go`

```go
package transfers

import "errors"

var (
    // ErrCacheMiss = no row with this idempotency_key. Caller should execute fresh.
    ErrCacheMiss = errors.New("idempotency_cache_miss")

    // ErrInFlight = row exists, status=pending. Caller should return 409 to client.
    ErrInFlight = errors.New("idempotency_in_flight")

    // ErrPayloadConflict = same key, different request_hash. Caller returns 422.
    ErrPayloadConflict = errors.New("idempotency_payload_conflict")
)
```

---

## 7. Module: `internal/transfers/store.go`

```go
package transfers

import (
    "bytes"
    "context"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "time"

    "github.com/google/uuid"
)

// Record is one row of the transfers table.
type Record struct {
    ID              uuid.UUID
    IdempotencyKey  string
    RequestHash     []byte
    RequestPayload  json.RawMessage
    ResponseStatus  *int             // null until completed
    ResponsePayload json.RawMessage  // null until completed
    Status          string           // pending | completed | failed
    TxnID           *uuid.UUID
    CreatedAt       time.Time
    CompletedAt     *time.Time
    ErrorMessage    *string
}

// LookupOrReserve checks for existing idempotency_key.
//
// Returns:
//   - (record, nil)             if found+completed AND request_hash matches
//   - (nil, ErrInFlight)        if found+pending
//   - (nil, ErrPayloadConflict) if found AND request_hash differs
//   - (nil, ErrCacheMiss)       if not found — caller should call Insert next
//
// On found+failed: DELETEs the row, returns ErrCacheMiss (treat as new).
func LookupOrReserve(ctx context.Context, dbx *sql.DB, key string, incomingHash []byte) (*Record, error) {
    var r Record
    err := dbx.QueryRowContext(ctx, `
        SELECT id, idempotency_key, request_hash, request_payload,
               response_status, response_payload, status,
               txn_id, created_at, completed_at, error_message
        FROM transfers
        WHERE idempotency_key = $1
    `, key).Scan(
        &r.ID, &r.IdempotencyKey, &r.RequestHash, &r.RequestPayload,
        &r.ResponseStatus, &r.ResponsePayload, &r.Status,
        &r.TxnID, &r.CreatedAt, &r.CompletedAt, &r.ErrorMessage,
    )
    if errors.Is(err, sql.ErrNoRows) {
        return nil, ErrCacheMiss
    }
    if err != nil {
        return nil, fmt.Errorf("lookup: %w", err)
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
        // delete and let caller execute fresh
        if _, err := dbx.ExecContext(ctx, `DELETE FROM transfers WHERE id = $1`, r.ID); err != nil {
            return nil, fmt.Errorf("delete failed row: %w", err)
        }
        return nil, ErrCacheMiss
    default:
        return nil, fmt.Errorf("unknown status: %s", r.Status)
    }
}

// Insert reserves the idempotency_key in pending state.
//
// On UNIQUE violation: returns ErrInFlight (another request reserved it first).
func Insert(ctx context.Context, dbx *sql.DB, key string, hash []byte, payload json.RawMessage) error {
    _, err := dbx.ExecContext(ctx, `
        INSERT INTO transfers (idempotency_key, request_hash, request_payload, status)
        VALUES ($1, $2, $3, 'pending')
    `, key, hash, payload)
    if err != nil {
        if isUniqueViolation(err) {
            return ErrInFlight
        }
        return fmt.Errorf("insert: %w", err)
    }
    return nil
}

// MarkCompleted records the response and links to the ledger txn_id.
func MarkCompleted(ctx context.Context, dbx *sql.DB, key string, txnID uuid.UUID, status int, body json.RawMessage) error {
    _, err := dbx.ExecContext(ctx, `
        UPDATE transfers
        SET status = 'completed',
            response_status = $1,
            response_payload = $2,
            txn_id = $3,
            completed_at = now()
        WHERE idempotency_key = $4 AND status = 'pending'
    `, status, body, txnID, key)
    if err != nil {
        return fmt.Errorf("mark completed: %w", err)
    }
    return nil
}

// MarkFailed records that ledger.Transfer returned an error.
// The row stays — on retry, LookupOrReserve will DELETE it and treat as cache miss.
func MarkFailed(ctx context.Context, dbx *sql.DB, key string, errMsg string) error {
    _, err := dbx.ExecContext(ctx, `
        UPDATE transfers
        SET status = 'failed',
            error_message = $1,
            completed_at = now()
        WHERE idempotency_key = $2 AND status = 'pending'
    `, errMsg, key)
    if err != nil {
        return fmt.Errorf("mark failed: %w", err)
    }
    return nil
}

// isUniqueViolation detects Postgres SQLSTATE 23505.
func isUniqueViolation(err error) bool {
    // pgx wraps errors; check for "duplicate key value" substring or use pgconn.PgError
    return err != nil && bytes.Contains([]byte(err.Error()), []byte("23505"))
}
```

---

## 8. Module: `internal/api/idempotency.go`

```go
package api

import (
    "bytes"
    "encoding/json"
    "errors"
    "io"
    "log/slog"
    "net/http"

    "github.com/google/uuid"

    "github.com/omkargade/distributed-payment-system/internal/transfers"
)

// IdempotencyMiddleware extracts Idempotency-Key, looks up the transfers table,
// and either serves a cached replay or forwards to the next handler.
//
// Wraps only POST /v1/transfer.
//
// Behavior:
//   - No header → generate UUID, no replay possible, fall through.
//   - Header + cache hit → write cached response + Idempotency-Replay: true header.
//   - Header + in-flight → 409 Conflict.
//   - Header + payload conflict → 422.
//   - Cache miss → INSERT pending, fall through with context carrying the key.
func (h *Handler) IdempotencyMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost || r.URL.Path != "/v1/transfer" {
            next.ServeHTTP(w, r)
            return
        }

        // Read body so we can hash + restore for downstream handler
        body, err := io.ReadAll(r.Body)
        if err != nil {
            writeJSON(w, http.StatusBadRequest, errorResponse{Error: "body_read_failed"})
            return
        }
        r.Body = io.NopCloser(bytes.NewReader(body))

        key := r.Header.Get("Idempotency-Key")
        if key == "" {
            key = uuid.NewString() // not retry-safe but operation works
        }

        // Hash the canonical JSON request
        var generic any
        if err := json.Unmarshal(body, &generic); err != nil {
            writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_json"})
            return
        }
        hash, err := transfers.HashCanonical(generic)
        if err != nil {
            writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
            return
        }

        // Lookup
        rec, err := transfers.LookupOrReserve(r.Context(), h.DB, key, hash)
        switch {
        case err == nil:
            // cache hit — replay
            slog.InfoContext(r.Context(), "transfer.replayed", "idempotency_key", key)
            w.Header().Set("Idempotency-Replay", "true")
            writeJSONRaw(w, *rec.ResponseStatus, rec.ResponsePayload)
            return
        case errors.Is(err, transfers.ErrInFlight):
            w.Header().Set("Retry-After", "1")
            writeJSON(w, http.StatusConflict, errorResponse{Error: "request_in_progress"})
            return
        case errors.Is(err, transfers.ErrPayloadConflict):
            slog.WarnContext(r.Context(), "idempotency.conflict", "idempotency_key", key)
            writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "idempotency_key_conflict"})
            return
        case errors.Is(err, transfers.ErrCacheMiss):
            // INSERT pending row
            if err := transfers.Insert(r.Context(), h.DB, key, hash, body); err != nil {
                if errors.Is(err, transfers.ErrInFlight) {
                    // race: another request reserved it between LookupOrReserve and Insert
                    w.Header().Set("Retry-After", "1")
                    writeJSON(w, http.StatusConflict, errorResponse{Error: "request_in_progress"})
                    return
                }
                writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
                return
            }
        default:
            slog.ErrorContext(r.Context(), "idempotency.lookup_failed", "error", err.Error())
            writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
            return
        }

        // Cache miss path: pass key + hash to handler via context
        ctx := contextWithIdempotency(r.Context(), key, hash)

        // Capture response so we can store it in transfers table after
        captureWriter := &captureResponseWriter{ResponseWriter: w, buf: &bytes.Buffer{}, status: http.StatusOK}
        next.ServeHTTP(captureWriter, r.WithContext(ctx))

        // Persist response back to transfers table (best-effort)
        // (in handler.Transfer we'll call transfers.MarkCompleted/MarkFailed
        // with the actual txn_id — middleware only writes the captured response)
    })
}

// captureResponseWriter is a tee'd writer — passes through to client AND buffers.
type captureResponseWriter struct {
    http.ResponseWriter
    buf    *bytes.Buffer
    status int
}

func (c *captureResponseWriter) WriteHeader(code int) {
    c.status = code
    c.ResponseWriter.WriteHeader(code)
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
    c.buf.Write(b)
    return c.ResponseWriter.Write(b)
}

func writeJSONRaw(w http.ResponseWriter, status int, body []byte) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    w.Write(body)
}

// context helpers
type idempotencyCtxKey struct{}

type idempotencyCtx struct {
    Key  string
    Hash []byte
}

func contextWithIdempotency(ctx context.Context, key string, hash []byte) context.Context {
    return context.WithValue(ctx, idempotencyCtxKey{}, idempotencyCtx{Key: key, Hash: hash})
}

func IdempotencyFromContext(ctx context.Context) (string, []byte, bool) {
    v, ok := ctx.Value(idempotencyCtxKey{}).(idempotencyCtx)
    if !ok {
        return "", nil, false
    }
    return v.Key, v.Hash, true
}
```

**Note:** the middleware reads + restores `r.Body` because the next handler also reads it. Stdlib `http.Request.Body` is a single-read stream; pattern is read-all → wrap in `io.NopCloser(bytes.NewReader(body))`.

---

## 9. Module: `internal/api/handler.go` (modified)

Key changes in `Transfer` handler:

```go
func (h *Handler) Transfer(w http.ResponseWriter, r *http.Request) {
    var req transferRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_json"})
        return
    }

    key, _, hasKey := IdempotencyFromContext(r.Context())

    slog.InfoContext(r.Context(), "transfer.received",
        "request_id", RequestIDFromContext(r.Context()),
        "idempotency_key", key,
        "payer_id", req.PayerID,
        "payee_id", req.PayeeID,
        "amount_minor", req.AmountMinor,
    )

    result, err := ledger.Transfer(r.Context(), h.DB, ledger.TransferRequest{
        PayerID:     req.PayerID,
        PayeeID:     req.PayeeID,
        AmountMinor: req.AmountMinor,
        Currency:    req.Currency,
    })

    if err != nil {
        // mark transfers row as failed (so retry can re-execute)
        if hasKey {
            _ = transfers.MarkFailed(r.Context(), h.DB, key, err.Error())
        }
        h.handleTransferError(w, r, err)
        return
    }

    // Build response body
    body, _ := json.Marshal(result)

    // Persist for future replay
    if hasKey {
        txnUUID, _ := uuid.Parse(result.TxnID)
        if err := transfers.MarkCompleted(r.Context(), h.DB, key, txnUUID, http.StatusOK, body); err != nil {
            slog.ErrorContext(r.Context(), "transfers.mark_completed_failed", "error", err.Error())
        }
    }

    writeJSONRaw(w, http.StatusOK, body)
}
```

**Critical change to `ledger.Transfer` (subtle):** we no longer want ledger.Transfer to generate its own `txn_id` randomly — for v2 idempotency, the `txn_id` should be derivable from the `idempotency_key` (so retries land on the same txn_id). Two options:

- **Option A** — pass `txnID` into `ledger.Transfer` as a parameter, derive in handler: `txnID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(key))`
- **Option B** — keep `txnID` random in ledger, store it in transfers table on first execute, retries serve cached response (which contains the original txnID)

**Recommended: Option B.** Simpler. The replay response includes the original txn_id, so client sees same txn_id on retry. No need to make txn_id deterministic.

So `ledger.Transfer` signature unchanged. Only handler logic changes.

---

## 10. Module: `internal/api/server.go` (modified)

```go
func NewServer(port int, dbx *sql.DB) *http.Server {
    h := &Handler{DB: dbx}

    mux := http.NewServeMux()
    mux.HandleFunc("GET /health", h.Health)
    mux.HandleFunc("POST /v1/transfer", h.Transfer)
    mux.HandleFunc("GET /v1/accounts/{id}", h.GetAccount)

    var handler http.Handler = mux
    handler = h.IdempotencyMiddleware(handler)     // NEW — innermost wrapper (closest to mux)
    handler = LoggingMiddleware(handler)
    handler = RequestIDMiddleware(handler)

    return &http.Server{
        Addr:    fmt.Sprintf(":%d", port),
        Handler: handler,
    }
}
```

Middleware order at runtime per request:
```
RequestID → Logging → Idempotency → mux → handler
```

Idempotency runs AFTER logging so the log line has `request_id` available. Idempotency runs BEFORE handler so cache hits skip the handler entirely.

---

## 11. Module: `cmd/payment-api/main.go` (modified)

```go
package main

import (
    "context"
    "errors"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/omkargade/distributed-payment-system/internal/api"
    "github.com/omkargade/distributed-payment-system/internal/config"
    "github.com/omkargade/distributed-payment-system/internal/db"
)

func main() {
    slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
        Level: slog.LevelInfo,
    })))

    cfg, err := config.Load()
    if err != nil {
        slog.Error("config.load_failed", "error", err.Error())
        os.Exit(1)
    }

    // Run migrations BEFORE opening pool — ensures schema is current
    if err := db.Migrate(cfg.DBURL); err != nil {
        slog.Error("db.migrate_failed", "error", err.Error())
        os.Exit(1)
    }
    slog.Info("db.migrate.complete")

    dbx, err := db.Open(cfg.DBURL)
    if err != nil {
        slog.Error("db.open_failed", "error", err.Error())
        os.Exit(1)
    }
    defer dbx.Close()

    srv := api.NewServer(cfg.Port, dbx)

    // Graceful shutdown wiring
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

    go func() {
        slog.Info("server.start", "svc", "payment-api", "port", cfg.Port)
        if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            slog.Error("server.listen_failed", "error", err.Error())
            os.Exit(1)
        }
    }()

    <-sigCh
    slog.Info("server.shutdown.starting")

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    if err := srv.Shutdown(ctx); err != nil {
        slog.Error("server.shutdown.error", "error", err.Error())
        os.Exit(1)
    }
    slog.Info("server.shutdown.complete")
}
```

---

## 12. Simulator changes — retry loop

### CLI flag additions
```
--retries=<int>             default 3 (1 initial + 2 retries)
--backoff-base-ms=<int>     default 200
--per-attempt-timeout=<dur> default 5s
```

### Per-worker loop changes (pseudocode)

```go
for range jobs {
    // Pick payer/payee/amount
    p1, p2 := pickPair(rng, accountIDs)
    amount := int64(rng.Intn(5000) + 1)
    intentKey := uuid.NewString()                  // one key per intent

    body, _ := json.Marshal(transferReq{...})

    for attempt := 1; attempt <= *retries; attempt++ {
        attemptStart := time.Now()
        reqCtx, cancel := context.WithTimeout(ctx, *perAttemptTimeout)
        req, _ := http.NewRequestWithContext(reqCtx, "POST", *target+"/v1/transfer", bytes.NewReader(body))
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Idempotency-Key", intentKey)

        resp, err := httpClient.Do(req)
        latency := time.Since(attemptStart).Milliseconds()
        cancel()

        rec := record{
            TS: time.Now().UTC().Format(time.RFC3339Nano),
            IdempotencyKey: intentKey,
            Attempt: attempt,
            Payer: p1, Payee: p2, Amount: amount,
            LatencyMs: latency,
        }

        if err != nil {
            rec.Status = 0
            rec.Error = err.Error()
        } else {
            rec.Status = resp.StatusCode
            if resp.Header.Get("Idempotency-Replay") == "true" {
                atomic.AddInt64(&replaysServed, 1)
            }
            io.Copy(io.Discard, resp.Body)
            resp.Body.Close()
        }

        shouldRetry := isRetryable(rec.Status, err)
        rec.Retried = shouldRetry && attempt < *retries
        rec.Final = !rec.Retried

        records <- rec
        atomic.AddInt64(&requestsTotal, 1)

        if !shouldRetry {
            // intent terminal (success or non-retryable failure)
            if rec.Status >= 200 && rec.Status < 300 {
                atomic.AddInt64(&intentsCompleted, 1)
            } else {
                atomic.AddInt64(&intentsFailed, 1)
            }
            break
        }

        if attempt >= *retries {
            atomic.AddInt64(&intentsFailed, 1)
            break
        }

        // Backoff with jitter
        sleep := time.Duration(*backoffBaseMs) * time.Millisecond * (1 << (attempt - 1))
        sleep = sleep + time.Duration(rng.Int63n(int64(sleep/2)))
        time.Sleep(sleep)
    }

    atomic.AddInt64(&intentsSent, 1)
}

func isRetryable(status int, err error) bool {
    if err != nil { return true }            // connection error
    if status == 0 { return true }
    if status >= 500 { return true }
    if status == 408 || status == 429 { return true }
    return false
}
```

### Summary output additions

```json
{
  "event": "simulator.summary",
  "intents_sent": 1500,
  "intents_completed": 1497,
  "intents_failed": 3,
  "requests_total": 1612,         // includes retries
  "replays_served": 89,           // count of Idempotency-Replay: true
  "p50_ms": 4, "p95_ms": 18, "p99_ms": 102,
  "duration_s": 30.1,
  "actual_intents_per_sec": 49.8,
  ...
}
```

---

## 13. Docker / Makefile changes

### `docker-compose.yml`

```yaml
services:
  postgres:
    image: postgres:16-alpine
    container_name: payment-postgres
    environment:
      POSTGRES_USER: payment
      POSTGRES_PASSWORD: payment_dev
      POSTGRES_DB: payment
      PGDATA: /var/lib/postgresql/data/pgdata
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
      # REMOVED: - ./migrations:/docker-entrypoint-initdb.d:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U payment -d payment"]
      interval: 2s
      timeout: 2s
      retries: 10
    restart: unless-stopped

  payment-api:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: payment-api
    environment:
      DB_URL: "postgres://payment:payment_dev@postgres:5432/payment?sslmode=disable"
      PORT: "8080"
      LOG_LEVEL: "info"
    ports:
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy
    restart: "no"
    stop_grace_period: 35s   # NEW: must exceed srv.Shutdown(30s) timeout

volumes:
  pgdata:
```

### `Makefile` additions

```makefile
RETRIES ?= 3

sim: build
	mkdir -p experiments/v2/data
	./bin/simulator \
	  --target=http://localhost:8080 \
	  --rps=$(RPS) --workers=$(WORKERS) --duration=$(DURATION) \
	  --retries=$(RETRIES) \
	  --output=experiments/v2/data/$(EXPERIMENT_ID)-sim-requests.jsonl

migrate-up: build
	./bin/payment-api  # migration runs on startup, then kill — TODO: add cmd/migrate binary if needed
```

### `Dockerfile` — embed migrations

Add `COPY migrations/ ./migrations/` BEFORE build step so `//go:embed` finds them:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/payment-api ./cmd/payment-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/payment-api /usr/local/bin/payment-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/payment-api"]
```

(No change — `COPY . .` already includes migrations/.)

---

## 14. New observability events

Add to existing log events:

| Event | Trigger | Fields added |
|-------|---------|--------------|
| `transfer.replayed` | Cache hit, serving cached response | `idempotency_key`, `original_txn_id` |
| `idempotency.conflict` | Same key + diff payload | `idempotency_key` |
| `idempotency.in_flight` | Same key, original still pending | `idempotency_key` |
| `db.migrate.complete` | App startup, migrations applied | `applied_count` (optional) |
| `server.shutdown.starting` | SIGTERM received | — |
| `server.shutdown.complete` | All handlers drained | `drain_duration_ms` |
| `transfers.mark_completed_failed` | UPDATE failed after ledger success | `idempotency_key`, `error` |

Modify existing events to include `idempotency_key` field when available:
- `transfer.received`
- `transfer.completed`
- `transfer.rejected`
- `transfer.failed`

---

## 15. v2 experiments — detailed

### Experiment 01: Duplicate request (rerun w/ idempotency)

```bash
make reset && sleep 10

KEY=$(uuidgen | tr 'A-Z' 'a-z')

# First request
curl -i -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Duplicate with same key
curl -i -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $KEY" \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Verify
curl -s http://localhost:8080/v1/accounts/acc_001     # expected: 50000 (charged once)
curl -s http://localhost:8080/v1/accounts/acc_002     # expected: 150000
make verify
```

**Expected:**
- Response 1: 200, txn_id=X
- Response 2: 200, txn_id=X (SAME), header `Idempotency-Replay: true`
- acc_001 balance = 50000 (single $500 debit, not double)
- Verifier passes
- Exactly 2 ledger rows (1 debit + 1 credit), not 4

### Experiment 02: App kill mid-txn (rerun w/ retry)

```bash
make reset && sleep 10
RPS=30 DURATION=60s RETRIES=3 EXPERIMENT_ID=02 make sim &
sleep 10 && docker kill payment-api
docker compose up -d payment-api && sleep 5
wait
make verify
```

**Expected:**
- Sim summary shows `intents_failed=0` or very near zero (vs v1's 1554 failures)
- Many `requests_total > intents_sent` (retries happened)
- `replays_served > 0` (some retries hit committed-before-kill transactions)
- Verifier passes

### Experiment 05: Postgres restart (rerun w/ retry)

```bash
make reset && sleep 10
RPS=30 DURATION=60s RETRIES=3 EXPERIMENT_ID=05 make sim &
sleep 10 && docker compose stop postgres
sleep 8 && docker compose up -d postgres
wait
make verify
```

**Expected:**
- `intents_failed=0` (or near zero — backoff fits within 5-8s downtime)
- High `requests_total / intents_sent` ratio (retries dominate during downtime)
- Verifier passes

### Experiment 06 (NEW): Payload conflict

```bash
make reset && sleep 10
KEY=$(uuidgen | tr 'A-Z' 'a-z')

# First — execute
curl -i -X POST http://localhost:8080/v1/transfer \
  -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Same key, DIFFERENT amount
curl -i -X POST http://localhost:8080/v1/transfer \
  -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":99999,"currency":"USD"}'

make verify
```

**Expected:**
- Response 1: 200
- Response 2: **422** with body `{"error":"idempotency_key_conflict"}`
- Only the $500 transfer hit the ledger
- Verifier passes
- App log shows `idempotency.conflict` event

### Experiment 07 (NEW): Concurrent same-key

```bash
make reset && sleep 10
KEY=$(uuidgen | tr 'A-Z' 'a-z')

# Fire 5 simultaneously
for i in 1 2 3 4 5; do
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H "Idempotency-Key: $KEY" \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}' &
done
wait

make verify
curl -s http://localhost:8080/v1/accounts/acc_001  # expected: 50000 (charged ONCE)
```

**Expected:**
- 1 response = 200 with txn_id=X
- 0-4 responses = 200 with `Idempotency-Replay: true` (txn_id=X) if first finished before others tried
- OR 0-4 responses = 409 `request_in_progress` if first still pending when others arrived
- Exactly 2 ledger rows total (1 debit + 1 credit)
- acc_001 charged ONCE ($500 → balance = 50000)
- Verifier passes

---

## 16. v2 done definition (mirrors v1's 7-section format)

### 1. Code complete
- [ ] `internal/transfers/` package: hash, store, errors
- [ ] `internal/api/idempotency.go` middleware
- [ ] `internal/api/handler.go` updated to mark completed/failed
- [ ] `internal/db/migrate.go` with embedded migrations
- [ ] `cmd/payment-api/main.go` with graceful shutdown + migrate
- [ ] `cmd/simulator/main.go` with retry loop + new counters
- [ ] All migrations 0001-0003 up/down pairs work

### 2. Happy path proven
- [ ] Curl with `Idempotency-Key` succeeds; second call replays with header
- [ ] Curl without key still works (server generates one)
- [ ] Verifier passes after standard sim run
- [ ] p99 still <100ms under normal load (idempotency adds 1 SELECT per request)

### 3. All 5 experiments documented
- [ ] 01 duplicate-request (rerun) — replay works, no double charge
- [ ] 02 process-kill (rerun w/ retry) — 0 intents lost
- [ ] 05 postgres-restart (rerun w/ retry) — 0 intents lost
- [ ] 06 payload-conflict — 422 returned, ledger unchanged
- [ ] 07 concurrent-same-key — exactly 1 execution

### 4. Observability
- [ ] `idempotency_key` field in all transfer log events
- [ ] New events: `transfer.replayed`, `idempotency.conflict`, `idempotency.in_flight`, `db.migrate.complete`, `server.shutdown.*`

### 5. Documentation
- [ ] `docs/v2-spec.md` matches built code
- [ ] `experiments/v2/README.md` indexes all 5
- [ ] `README.md` updated with v2 quickstart + Idempotency-Key example

### 6. Migration reliability
- [ ] `make reset` (fresh DB) applies all migrations cleanly in <5s
- [ ] `docker compose restart payment-api` against existing DB skips already-applied migrations
- [ ] `golang-migrate` down works (verify `0003_transfers.down.sql` removes table)

### 7. Reflection captured
- [ ] `experiments/v2/reflection.md` written

### Estimated time
~70% of v1 effort. ~3-5 evenings or 1 weekend.

### Trigger to v3
All 7 checked → `git tag v2` → start `docs/v3-spec.md`.

---

## 17. Out of scope (v2 — DO NOT add)

- Race condition fix (`SELECT FOR UPDATE`) → v3
- Connection pool tuning → v3
- Indexes on ledger_entries → v3
- Outbox table for event emission → v5
- Prometheus / metrics endpoint → v4
- Distributed tracing → v7
- Background cleanup of expired idempotency keys → v4 (needs worker queue)
- Multi-currency → never
- Auth → never (still single-user toy)
- Read replicas → v6

---

## 18. Spec ↔ code drift policy

This spec is binding for v2. If implementation diverges, EITHER:
- Update the spec to match (if divergence is a real improvement), OR
- Fix the code to match (if divergence is incidental)

Drift = bug. Catch during "done" checklist review.
