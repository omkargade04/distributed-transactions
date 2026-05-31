# v1 Specification — Distributed Payment System

**Project:** Learning Distributed Systems via Payment System
**Version:** v1 — primitive monolith (designed to crash)
**Status:** Implementation-ready
**Source decisions:** `V1-GRILL-DECISIONS.md`
**Date:** 2026-05-23

---

## 0. Overview

v1 ships a deliberately primitive monolith. Single Go service handles P2P wallet transfers against Postgres. Two helper binaries (simulator, verifier) drive load and check invariants. Docker Compose runs the stack.

v1 has no idempotency, no retries, no metrics, no pooling tuning, no indexes beyond PK, no auth, no graceful shutdown. It will break in 5 documented ways. Each break maps to a lesson in v2–v8.

---

## 1. Architecture

```mermaid
graph TD
    Sim[Simulator binary<br/>concurrent workers] -->|HTTP POST /transfer| API[payment-api<br/>Go HTTP server]
    API -->|SQL| DB[(Postgres 16<br/>accounts + ledger_entries)]
    Verifier[Verifier binary<br/>invariant checker] -->|SQL read-only| DB
    API -->|stdout JSON logs| Logs[Docker logs]

    style API fill:#dae8fc
    style DB fill:#d5e8d4
    style Sim fill:#fff2cc
    style Verifier fill:#f8cecc
```

**Process boundaries:**
- `payment-api` — long-running HTTP server (container)
- `postgres` — DB (container)
- `simulator` — short-lived load generator (host or container)
- `verifier` — short-lived invariant check (host or container)

**Data flow per transfer:**
1. Simulator POSTs `/v1/transfer`
2. Handler validates payload
3. Handler opens DB txn (`BEGIN`)
4. Handler reads payer balance
5. Handler validates `balance >= amount`
6. Handler UPDATEs payer balance (subtract)
7. Handler UPDATEs payee balance (add)
8. Handler INSERTs 2 ledger_entries (debit + credit, same txn_id)
9. Handler `COMMIT`s
10. Handler returns 200 with txn_id

---

## 2. Repository Layout

```
distributed-payment-system/
├── README.md
├── docker-compose.yml
├── Makefile
├── Dockerfile
├── .gitignore
├── .env.example
├── go.mod
├── go.sum
│
├── cmd/
│   ├── payment-api/main.go
│   ├── simulator/main.go
│   └── verifier/main.go
│
├── internal/
│   ├── api/
│   │   ├── server.go
│   │   ├── handler.go
│   │   └── middleware.go
│   ├── ledger/
│   │   ├── transfer.go
│   │   ├── account.go
│   │   └── errors.go
│   ├── db/
│   │   ├── db.go
│   │   └── queries.go
│   └── config/
│       └── config.go
│
├── migrations/
│   ├── 001_init.sql
│   └── 002_seed.sql
│
├── experiments/v1/
│   ├── README.md
│   ├── 01-duplicate-request.md
│   ├── 02-process-kill.md
│   ├── 03-concurrent-race.md
│   ├── 04-pool-exhaustion.md
│   ├── 05-postgres-restart.md
│   ├── reflection.md
│   ├── data/             # gitignored (raw simulator outputs)
│   └── _assets/{report.css, chart-helpers.js}
│
├── docs/
│   └── v1-spec.md        # this document, copied into repo
│
├── templates/
│   ├── experiment.html
│   └── experiment-template.md
│
└── scripts/
    ├── seed.sh
    ├── reset-db.sh
    ├── psql.sh
    ├── new-experiment.sh
    ├── render-experiment.sh
    └── render-all.sh
```

---

## 3. Module: `internal/config`

### `internal/config/config.go`

```go
package config

import (
    "fmt"
    "os"
    "strconv"
)

type Config struct {
    DBURL    string
    Port     int
    LogLevel string
}

func Load() (*Config, error) {
    cfg := &Config{
        DBURL:    os.Getenv("DB_URL"),
        LogLevel: getEnvDefault("LOG_LEVEL", "info"),
    }
    if cfg.DBURL == "" {
        return nil, fmt.Errorf("DB_URL required")
    }
    portStr := getEnvDefault("PORT", "8080")
    port, err := strconv.Atoi(portStr)
    if err != nil {
        return nil, fmt.Errorf("invalid PORT: %w", err)
    }
    cfg.Port = port
    return cfg, nil
}

func getEnvDefault(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}
```

---

## 4. Module: `internal/db`

### `internal/db/db.go`

```go
package db

import (
    "database/sql"
    "fmt"

    _ "github.com/jackc/pgx/v5/stdlib"
)

func Open(dbURL string) (*sql.DB, error) {
    db, err := sql.Open("pgx", dbURL)
    if err != nil {
        return nil, fmt.Errorf("sql.Open: %w", err)
    }
    if err := db.Ping(); err != nil {
        return nil, fmt.Errorf("db.Ping: %w", err)
    }
    // NO pool tuning in v1 — defaults used deliberately.
    // database/sql defaults: MaxOpenConns=0 (unlimited), MaxIdleConns=2.
    // Postgres max_connections=100 → expect pool exhaustion above ~50 RPS sustained.
    return db, nil
}
```

### `internal/db/queries.go`

```go
package db

const (
    QGetAccount = `
        SELECT id, balance_minor, currency
        FROM accounts
        WHERE id = $1
    `

    QUpdateBalance = `
        UPDATE accounts
        SET balance_minor = balance_minor + $1,
            updated_at    = now()
        WHERE id = $2
    `

    QInsertLedgerEntry = `
        INSERT INTO ledger_entries (txn_id, account_id, amount_minor)
        VALUES ($1, $2, $3)
    `
)
```

All SQL is parameterized. No string concatenation with user input ever.

---

## 5. Module: `internal/ledger`

### `internal/ledger/errors.go`

```go
package ledger

import "errors"

var (
    ErrAccountNotFound   = errors.New("account_not_found")
    ErrInsufficientFunds = errors.New("insufficient_funds")
    ErrSamePayerPayee    = errors.New("same_payer_payee")
    ErrInvalidAmount     = errors.New("invalid_amount")
)
```

### `internal/ledger/account.go`

```go
package ledger

import (
    "context"
    "database/sql"
    "errors"
    "fmt"

    "github.com/yourname/distributed-payment-system/internal/db"
)

type Account struct {
    ID           string
    BalanceMinor int64
    Currency     string
}

func GetAccount(ctx context.Context, dbx *sql.DB, id string) (*Account, error) {
    var a Account
    err := dbx.QueryRowContext(ctx, db.QGetAccount, id).Scan(
        &a.ID, &a.BalanceMinor, &a.Currency,
    )
    if errors.Is(err, sql.ErrNoRows) {
        return nil, ErrAccountNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("get account: %w", err)
    }
    return &a, nil
}
```

### `internal/ledger/transfer.go`

This is the heart of v1. Five DB statements in one transaction:

```go
package ledger

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "log/slog"
    "time"

    "github.com/google/uuid"
    "github.com/yourname/distributed-payment-system/internal/db"
)

type TransferRequest struct {
    PayerID     string
    PayeeID     string
    AmountMinor int64
    Currency    string
}

type TransferResult struct {
    TxnID  string
    Status string
}

func Transfer(ctx context.Context, dbx *sql.DB, req TransferRequest) (*TransferResult, error) {
    // Validate input
    if req.AmountMinor <= 0 {
        return nil, ErrInvalidAmount
    }
    if req.PayerID == req.PayeeID {
        return nil, ErrSamePayerPayee
    }

    txnID := uuid.New()
    start := time.Now()

    tx, err := dbx.BeginTx(ctx, nil) // default isolation (READ COMMITTED in Postgres)
    if err != nil {
        return nil, fmt.Errorf("begin: %w", err)
    }
    defer tx.Rollback()

    // Read payer balance
    var payerBal int64
    err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayerID).Scan(
        new(string), &payerBal, new(string),
    )
    if errors.Is(err, sql.ErrNoRows) {
        return nil, ErrAccountNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("read payer: %w", err)
    }

    // Read payee (existence check)
    var payeeBal int64
    err = tx.QueryRowContext(ctx, db.QGetAccount, req.PayeeID).Scan(
        new(string), &payeeBal, new(string),
    )
    if errors.Is(err, sql.ErrNoRows) {
        return nil, ErrAccountNotFound
    }
    if err != nil {
        return nil, fmt.Errorf("read payee: %w", err)
    }

    // Balance check
    // NOTE: This check is the race condition lab.
    // Two concurrent transfers from same payer can both pass this check.
    // v3 lesson: SELECT FOR UPDATE or higher isolation.
    if payerBal < req.AmountMinor {
        return nil, ErrInsufficientFunds
    }

    slog.DebugContext(ctx, "transfer.validated",
        "txn_id", txnID, "payer_balance", payerBal,
    )

    // Debit payer
    if _, err = tx.ExecContext(ctx, db.QUpdateBalance, -req.AmountMinor, req.PayerID); err != nil {
        return nil, fmt.Errorf("debit payer: %w", err)
    }

    // Credit payee
    if _, err = tx.ExecContext(ctx, db.QUpdateBalance, req.AmountMinor, req.PayeeID); err != nil {
        return nil, fmt.Errorf("credit payee: %w", err)
    }

    // Insert debit ledger entry
    if _, err = tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayerID, -req.AmountMinor); err != nil {
        return nil, fmt.Errorf("ledger debit: %w", err)
    }

    // Insert credit ledger entry
    if _, err = tx.ExecContext(ctx, db.QInsertLedgerEntry, txnID, req.PayeeID, req.AmountMinor); err != nil {
        return nil, fmt.Errorf("ledger credit: %w", err)
    }

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
```

---

## 6. Module: `internal/api`

### `internal/api/middleware.go`

```go
package api

import (
    "context"
    "log/slog"
    "net/http"
    "time"

    "github.com/google/uuid"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

func RequestIDMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        reqID := r.Header.Get("X-Request-ID")
        if reqID == "" {
            reqID = uuid.NewString()
        }
        ctx := context.WithValue(r.Context(), requestIDKey, reqID)
        w.Header().Set("X-Request-ID", reqID)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func LoggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        reqID, _ := r.Context().Value(requestIDKey).(string)

        slog.InfoContext(r.Context(), "request.received",
            "request_id", reqID,
            "method", r.Method,
            "path", r.URL.Path,
        )

        sw := &statusWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(sw, r)

        slog.InfoContext(r.Context(), "request.completed",
            "request_id", reqID,
            "method", r.Method,
            "path", r.URL.Path,
            "status", sw.status,
            "duration_ms", time.Since(start).Milliseconds(),
        )
    })
}

type statusWriter struct {
    http.ResponseWriter
    status int
}

func (sw *statusWriter) WriteHeader(code int) {
    sw.status = code
    sw.ResponseWriter.WriteHeader(code)
}

func RequestIDFromContext(ctx context.Context) string {
    v, _ := ctx.Value(requestIDKey).(string)
    return v
}
```

### `internal/api/handler.go`

```go
package api

import (
    "database/sql"
    "encoding/json"
    "errors"
    "log/slog"
    "net/http"

    "github.com/yourname/distributed-payment-system/internal/ledger"
)

type Handler struct {
    DB *sql.DB
}

type transferRequest struct {
    PayerID     string `json:"payer_id"`
    PayeeID     string `json:"payee_id"`
    AmountMinor int64  `json:"amount_minor"`
    Currency    string `json:"currency"`
}

type errorResponse struct {
    Error string `json:"error"`
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) Transfer(w http.ResponseWriter, r *http.Request) {
    var req transferRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_json"})
        return
    }

    slog.InfoContext(r.Context(), "transfer.received",
        "request_id", RequestIDFromContext(r.Context()),
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
        h.handleTransferError(w, r, err)
        return
    }

    writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleTransferError(w http.ResponseWriter, r *http.Request, err error) {
    switch {
    case errors.Is(err, ledger.ErrAccountNotFound):
        slog.InfoContext(r.Context(), "transfer.rejected", "reason", "account_not_found")
        writeJSON(w, http.StatusNotFound, errorResponse{Error: "account_not_found"})
    case errors.Is(err, ledger.ErrInsufficientFunds):
        slog.InfoContext(r.Context(), "transfer.rejected", "reason", "insufficient_funds")
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "insufficient_funds"})
    case errors.Is(err, ledger.ErrSamePayerPayee):
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "same_payer_payee"})
    case errors.Is(err, ledger.ErrInvalidAmount):
        writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid_amount"})
    default:
        slog.ErrorContext(r.Context(), "transfer.failed", "error", err.Error())
        writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
    }
}

func (h *Handler) GetAccount(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    acc, err := ledger.GetAccount(r.Context(), h.DB, id)
    if errors.Is(err, ledger.ErrAccountNotFound) {
        writeJSON(w, http.StatusNotFound, errorResponse{Error: "account_not_found"})
        return
    }
    if err != nil {
        slog.ErrorContext(r.Context(), "account.lookup_failed", "error", err.Error())
        writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
        return
    }
    writeJSON(w, http.StatusOK, map[string]any{
        "id":            acc.ID,
        "balance_minor": acc.BalanceMinor,
        "currency":      acc.Currency,
    })
}

func writeJSON(w http.ResponseWriter, status int, body any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(body)
}
```

### `internal/api/server.go`

```go
package api

import (
    "database/sql"
    "fmt"
    "net/http"
)

func NewServer(port int, dbx *sql.DB) *http.Server {
    h := &Handler{DB: dbx}

    mux := http.NewServeMux()
    mux.HandleFunc("GET /health", h.Health)
    mux.HandleFunc("POST /v1/transfer", h.Transfer)
    mux.HandleFunc("GET /v1/accounts/{id}", h.GetAccount)

    var handler http.Handler = mux
    handler = LoggingMiddleware(handler)
    handler = RequestIDMiddleware(handler)

    return &http.Server{
        Addr:    fmt.Sprintf(":%d", port),
        Handler: handler,
    }
}
```

---

## 7. Binary: `cmd/payment-api`

### `cmd/payment-api/main.go`

```go
package main

import (
    "log/slog"
    "os"

    "github.com/yourname/distributed-payment-system/internal/api"
    "github.com/yourname/distributed-payment-system/internal/config"
    "github.com/yourname/distributed-payment-system/internal/db"
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

    dbx, err := db.Open(cfg.DBURL)
    if err != nil {
        slog.Error("db.open_failed", "error", err.Error())
        os.Exit(1)
    }
    defer dbx.Close()

    srv := api.NewServer(cfg.Port, dbx)

    slog.Info("server.start",
        "svc", "payment-api",
        "port", cfg.Port,
    )

    // NO graceful shutdown in v1 — deliberate.
    // SIGTERM kills in-flight requests. v2 lesson.
    if err := srv.ListenAndServe(); err != nil {
        slog.Error("server.shutdown", "error", err.Error())
        os.Exit(1)
    }
}
```

---

## 8. Binary: `cmd/simulator`

### `cmd/simulator/main.go`

```go
package main

import (
    "bytes"
    "context"
    "encoding/json"
    "flag"
    "fmt"
    "io"
    "log/slog"
    "math/rand"
    "net/http"
    "os"
    "sort"
    "sync"
    "sync/atomic"
    "time"
)

type transferReq struct {
    PayerID     string `json:"payer_id"`
    PayeeID     string `json:"payee_id"`
    AmountMinor int64  `json:"amount_minor"`
    Currency    string `json:"currency"`
}

type record struct {
    TS        string `json:"ts"`
    Payer     string `json:"payer"`
    Payee     string `json:"payee"`
    Amount    int64  `json:"amount"`
    Status    int    `json:"status"`
    LatencyMs int64  `json:"latency_ms"`
    Error     string `json:"error,omitempty"`
}

func main() {
    target := flag.String("target", "http://localhost:8080", "payment-api URL")
    rps := flag.Int("rps", 100, "target requests per second")
    workers := flag.Int("workers", 10, "concurrent worker count")
    duration := flag.Duration("duration", 60*time.Second, "run duration")
    seed := flag.Int64("seed", time.Now().UnixNano(), "random seed for reproducibility")
    accounts := flag.Int("accounts", 100, "size of pre-seeded account pool")
    output := flag.String("output", "", "JSONL output path (optional)")
    flag.Parse()

    slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

    var outFile *os.File
    if *output != "" {
        f, err := os.Create(*output)
        if err != nil {
            slog.Error("output.create_failed", "error", err.Error())
            os.Exit(1)
        }
        defer f.Close()
        outFile = f
    }

    rng := rand.New(rand.NewSource(*seed))
    accountIDs := make([]string, *accounts)
    for i := 0; i < *accounts; i++ {
        accountIDs[i] = fmt.Sprintf("acc_%03d", i+1)
    }

    ctx, cancel := context.WithTimeout(context.Background(), *duration)
    defer cancel()

    httpClient := &http.Client{Timeout: 10 * time.Second}

    // Token bucket: tick at 1/RPS interval
    tickInterval := time.Second / time.Duration(*rps)
    ticker := time.NewTicker(tickInterval)
    defer ticker.Stop()

    var (
        sent           int64
        completed2xx   int64
        rejected4xx    int64
        failed5xx      int64
        latenciesMu    sync.Mutex
        latencies      []int64
        recordsCh      = make(chan record, 1024)
    )

    // Writer goroutine
    var writerWg sync.WaitGroup
    writerWg.Add(1)
    go func() {
        defer writerWg.Done()
        for r := range recordsCh {
            if outFile != nil {
                b, _ := json.Marshal(r)
                outFile.Write(b)
                outFile.WriteString("\n")
            }
        }
    }()

    // Worker pool
    var wg sync.WaitGroup
    jobs := make(chan struct{}, *workers*2)

    for w := 0; w < *workers; w++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for range jobs {
                p1 := accountIDs[rng.Intn(len(accountIDs))]
                p2 := accountIDs[rng.Intn(len(accountIDs))]
                for p1 == p2 {
                    p2 = accountIDs[rng.Intn(len(accountIDs))]
                }
                amount := int64(rng.Intn(5000) + 1) // 1¢ to $50

                body, _ := json.Marshal(transferReq{
                    PayerID: p1, PayeeID: p2,
                    AmountMinor: amount, Currency: "USD",
                })

                start := time.Now()
                resp, err := httpClient.Post(
                    *target+"/v1/transfer",
                    "application/json",
                    bytes.NewReader(body),
                )
                latency := time.Since(start).Milliseconds()

                rec := record{
                    TS: time.Now().UTC().Format(time.RFC3339Nano),
                    Payer: p1, Payee: p2, Amount: amount,
                    LatencyMs: latency,
                }

                if err != nil {
                    rec.Status = 0
                    rec.Error = err.Error()
                    atomic.AddInt64(&failed5xx, 1)
                } else {
                    rec.Status = resp.StatusCode
                    io.Copy(io.Discard, resp.Body)
                    resp.Body.Close()
                    switch {
                    case resp.StatusCode >= 200 && resp.StatusCode < 300:
                        atomic.AddInt64(&completed2xx, 1)
                    case resp.StatusCode >= 400 && resp.StatusCode < 500:
                        atomic.AddInt64(&rejected4xx, 1)
                    default:
                        atomic.AddInt64(&failed5xx, 1)
                    }
                }

                latenciesMu.Lock()
                latencies = append(latencies, latency)
                latenciesMu.Unlock()

                recordsCh <- rec
                atomic.AddInt64(&sent, 1)
            }
        }()
    }

    // Dispatcher
    runStart := time.Now()
    go func() {
        for {
            select {
            case <-ctx.Done():
                close(jobs)
                return
            case <-ticker.C:
                select {
                case jobs <- struct{}{}:
                default:
                    // worker pool full, drop tick (backpressure)
                }
            }
        }
    }()

    wg.Wait()
    close(recordsCh)
    writerWg.Wait()

    totalDuration := time.Since(runStart)

    // Compute latency percentiles
    latenciesMu.Lock()
    sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
    p50 := percentile(latencies, 50)
    p95 := percentile(latencies, 95)
    p99 := percentile(latencies, 99)
    latenciesMu.Unlock()

    // Print summary
    summary := map[string]any{
        "event":          "simulator.summary",
        "sent":           atomic.LoadInt64(&sent),
        "completed_2xx":  atomic.LoadInt64(&completed2xx),
        "rejected_4xx":   atomic.LoadInt64(&rejected4xx),
        "failed_5xx":     atomic.LoadInt64(&failed5xx),
        "p50_ms":         p50,
        "p95_ms":         p95,
        "p99_ms":         p99,
        "duration_s":     totalDuration.Seconds(),
        "actual_rps":     float64(sent) / totalDuration.Seconds(),
        "seed":           *seed,
    }
    b, _ := json.MarshalIndent(summary, "", "  ")
    fmt.Println(string(b))
}

func percentile(sorted []int64, p int) int64 {
    if len(sorted) == 0 {
        return 0
    }
    idx := (p * len(sorted)) / 100
    if idx >= len(sorted) {
        idx = len(sorted) - 1
    }
    return sorted[idx]
}
```

---

## 9. Binary: `cmd/verifier`

### `cmd/verifier/main.go`

```go
package main

import (
    "database/sql"
    "encoding/json"
    "flag"
    "fmt"
    "log/slog"
    "os"

    _ "github.com/jackc/pgx/v5/stdlib"
)

type Result struct {
    LedgerSum             int64    `json:"ledger_sum"`              // I1: must = 0
    DriftedAccounts       []string `json:"drifted_accounts"`        // I2: balance != initial + SUM(ledger)
    NegativeBalanceAccts  []string `json:"negative_balance_accts"`  // app invariant
    OrphanTxnIDs          []string `json:"orphan_txn_ids"`          // I3: txn_id without 2 rows
    Pass                  bool     `json:"pass"`
}

func main() {
    dbURL := flag.String("db", os.Getenv("DB_URL"), "postgres DSN")
    initialBalance := flag.Int64("initial-balance", 100000, "seeded balance per account")
    flag.Parse()

    slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

    if *dbURL == "" {
        fmt.Fprintln(os.Stderr, "--db or DB_URL required")
        os.Exit(2)
    }

    dbx, err := sql.Open("pgx", *dbURL)
    if err != nil {
        slog.Error("db.open", "error", err.Error())
        os.Exit(2)
    }
    defer dbx.Close()

    res := Result{Pass: true}

    // I1: ledger sum = 0
    if err := dbx.QueryRow(`SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&res.LedgerSum); err != nil {
        slog.Error("query.ledger_sum", "error", err.Error())
        os.Exit(2)
    }
    if res.LedgerSum != 0 {
        res.Pass = false
    }

    // I2: balance drift
    rows, err := dbx.Query(`
        SELECT a.id
        FROM accounts a
        LEFT JOIN ledger_entries l ON l.account_id = a.id
        GROUP BY a.id, a.balance_minor
        HAVING a.balance_minor != COALESCE(SUM(l.amount_minor), 0) + $1
    `, *initialBalance)
    if err != nil {
        slog.Error("query.drift", "error", err.Error())
        os.Exit(2)
    }
    for rows.Next() {
        var id string
        rows.Scan(&id)
        res.DriftedAccounts = append(res.DriftedAccounts, id)
    }
    rows.Close()
    if len(res.DriftedAccounts) > 0 {
        res.Pass = false
    }

    // Negative balances
    rows, err = dbx.Query(`SELECT id FROM accounts WHERE balance_minor < 0`)
    if err != nil {
        slog.Error("query.negative", "error", err.Error())
        os.Exit(2)
    }
    for rows.Next() {
        var id string
        rows.Scan(&id)
        res.NegativeBalanceAccts = append(res.NegativeBalanceAccts, id)
    }
    rows.Close()
    if len(res.NegativeBalanceAccts) > 0 {
        res.Pass = false
    }

    // I3: orphan txn_ids
    rows, err = dbx.Query(`
        SELECT txn_id::text FROM ledger_entries
        GROUP BY txn_id HAVING COUNT(*) != 2
    `)
    if err != nil {
        slog.Error("query.orphan", "error", err.Error())
        os.Exit(2)
    }
    for rows.Next() {
        var id string
        rows.Scan(&id)
        res.OrphanTxnIDs = append(res.OrphanTxnIDs, id)
    }
    rows.Close()
    if len(res.OrphanTxnIDs) > 0 {
        res.Pass = false
    }

    // Pretty-print summary
    fmt.Println("=== Verifier Report ===")
    printCheck("I1 ledger sum == 0", res.LedgerSum == 0, fmt.Sprintf("sum=%d", res.LedgerSum))
    printCheck("I2 no balance drift", len(res.DriftedAccounts) == 0, fmt.Sprintf("drifted=%v", res.DriftedAccounts))
    printCheck("    no negative balances", len(res.NegativeBalanceAccts) == 0, fmt.Sprintf("negative=%v", res.NegativeBalanceAccts))
    printCheck("I3 all txns have 2 entries", len(res.OrphanTxnIDs) == 0, fmt.Sprintf("orphans=%v", res.OrphanTxnIDs))

    // Emit JSON for HTML reports
    b, _ := json.MarshalIndent(res, "", "  ")
    fmt.Println("\n--- JSON ---")
    fmt.Println(string(b))

    if !res.Pass {
        os.Exit(1)
    }
}

func printCheck(name string, ok bool, detail string) {
    mark := "✓"
    if !ok {
        mark = "✗"
    }
    fmt.Printf("  %s %s  %s\n", mark, name, detail)
}
```

---

## 10. Migrations

### `migrations/001_init.sql`

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS accounts (
    id            TEXT PRIMARY KEY,
    balance_minor BIGINT NOT NULL,
    currency      CHAR(3) NOT NULL DEFAULT 'USD',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    txn_id       UUID NOT NULL,
    account_id   TEXT NOT NULL REFERENCES accounts(id),
    amount_minor BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- DELIBERATELY no indexes beyond PK in v1.
-- DELIBERATELY no CHECK (balance_minor >= 0) — must demonstrate race condition.
```

### `migrations/002_seed.sql`

```sql
INSERT INTO accounts (id, balance_minor)
SELECT 'acc_' || lpad(g::text, 3, '0'), 100000
FROM generate_series(1, 100) AS g
ON CONFLICT (id) DO NOTHING;
```

---

## 11. Docker & Build

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
      - ./migrations:/docker-entrypoint-initdb.d:ro
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
    restart: "no"   # DELIBERATE: crashes must stay crashed in v1

volumes:
  pgdata:
```

### `Dockerfile`

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/payment-api ./cmd/payment-api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/payment-api /usr/local/bin/payment-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/payment-api"]
```

### `Makefile`

```makefile
.PHONY: help build up down logs sim verify reset psql render-v1 clean

DB_URL ?= postgres://payment:payment_dev@localhost:5432/payment?sslmode=disable
RPS ?= 50
WORKERS ?= 10
DURATION ?= 30s
EXPERIMENT_ID ?= 00

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build all Go binaries to ./bin
	mkdir -p bin
	go build -o bin/payment-api ./cmd/payment-api
	go build -o bin/simulator   ./cmd/simulator
	go build -o bin/verifier    ./cmd/verifier

up: ## Bring up stack
	docker compose up -d --build

down: ## Stop stack (keep data)
	docker compose down

logs: ## Tail payment-api logs through jq
	docker logs -f payment-api | jq .

sim: build ## Run simulator (RPS, WORKERS, DURATION, EXPERIMENT_ID)
	mkdir -p experiments/v1/data
	./bin/simulator \
	  --target=http://localhost:8080 \
	  --rps=$(RPS) --workers=$(WORKERS) --duration=$(DURATION) \
	  --output=experiments/v1/data/$(EXPERIMENT_ID)-sim-requests.jsonl

verify: build ## Run verifier
	./bin/verifier --db="$(DB_URL)"

reset: ## Nuke DB volume and restart
	docker compose down -v
	docker compose up -d --build

psql: ## Open psql shell
	docker exec -it payment-postgres psql -U payment -d payment

render-v1: ## Generate HTML reports for all v1 experiments
	bash scripts/render-all.sh experiments/v1

clean: ## Remove build artifacts
	rm -rf bin/
```

### `.env.example`

```
DB_URL=postgres://payment:payment_dev@localhost:5432/payment?sslmode=disable
PORT=8080
LOG_LEVEL=info
```

### `.gitignore`

```
/bin/
/experiments/*/data/*.jsonl
/experiments/*/data/*.sql
/experiments/*/*.html
/experiments/*/index.html
.env
*.log
.DS_Store
```

### `go.mod` dependencies

```go
module github.com/yourname/distributed-payment-system

go 1.23

require (
    github.com/google/uuid v1.6.0
    github.com/jackc/pgx/v5 v5.7.0
)
```

Only 2 direct deps. No web framework, no ORM.

---

## 12. Failure Mode Reproduction Recipes

Each must produce evidence that goes into `experiments/v1/0N-*.md`.

### 01 — Duplicate request

```bash
# Setup
make reset && sleep 5

# Send identical transfer twice
curl -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

curl -X POST http://localhost:8080/v1/transfer \
  -H 'Content-Type: application/json' \
  -d '{"payer_id":"acc_001","payee_id":"acc_002","amount_minor":50000,"currency":"USD"}'

# Verify
make verify
# Expected: acc_001 balance = 0 (was $1000, charged $1000 total via 2 distinct txns).
# Bug: client retried, no idempotency_key, system has no way to detect duplicate intent.
# Both txns produce 2 ledger rows each → SUM still = 0 (ledger sane).
# I1 PASSES. The bug is in business logic — paid twice for "the same" intent.
```

### 02 — Process kill mid-txn

```bash
make reset && sleep 5

# Start moderate load
EXPERIMENT_ID=02 RPS=30 DURATION=60s make sim &
SIM_PID=$!

sleep 15
docker kill payment-api

wait $SIM_PID
docker compose up -d payment-api
sleep 5

make verify
# Expected: in-flight requests at kill time got connection-refused / 5xx.
# DB txns either fully committed or fully rolled back (Postgres ACID).
# I1, I2, I3 should all PASS.
# The failure is OBSERVABLE — simulator records 5xx, but DB stays consistent.
# Lesson preview (v2): persistent retry queue / outbox so in-flight intents survive.
```

### 03 — Concurrent same-payer race

```bash
make reset && sleep 5

# Drain acc_001 with 2 concurrent transfers that should not both succeed
# Each transfers $600. acc_001 only has $1000. One must fail.
# With READ COMMITTED + no FOR UPDATE, both can pass balance check.

cat > /tmp/race.sh << 'EOF'
#!/bin/bash
for i in 1 2; do
  curl -s -X POST http://localhost:8080/v1/transfer \
    -H 'Content-Type: application/json' \
    -d '{"payer_id":"acc_001","payee_id":"acc_00'$((i+1))'","amount_minor":60000,"currency":"USD"}' &
done
wait
EOF
chmod +x /tmp/race.sh

# Run race many times to surface it (timing-dependent)
for r in $(seq 1 20); do
  make reset >/dev/null && sleep 3
  /tmp/race.sh
  result=$(./bin/verifier --db="$DB_URL" 2>/dev/null)
  if echo "$result" | grep -q "negative_balance"; then
    echo "Race triggered at attempt $r"
    break
  fi
done

make verify
# Expected after race: acc_001 balance = -20000 (both transfers succeeded).
# I1 ledger sum still = 0 (ACID held within each txn).
# I2 balance/ledger consistent (UPDATE + INSERT atomic within each txn).
# But invariant balance >= 0 (app rule, not DB constraint) is broken.
```

### 04 — DB connection pool exhaustion

```bash
make reset && sleep 5

# Crank simulator past Postgres max_connections (default 100)
EXPERIMENT_ID=04 RPS=500 WORKERS=200 DURATION=60s make sim

# Watch app logs
docker logs payment-api | jq 'select(.level=="error")' | head -20

# Expected:
# - Many requests return 500 with "too many connections" or query timeout
# - app log shows "db.error: pq: sorry, too many clients already" or similar
# - I1, I2 still hold (failed txns rolled back cleanly)
# Lesson: tune Postgres max_connections, add pgbouncer, OR limit pool in app.
```

### 05 — Postgres restart mid-load

```bash
make reset && sleep 5

EXPERIMENT_ID=05 RPS=30 DURATION=90s make sim &
SIM_PID=$!

sleep 20
docker restart payment-postgres

wait $SIM_PID
sleep 5
make verify

# Expected:
# - During Postgres downtime: app returns 500s ("connection refused")
# - When Postgres returns: app does NOT auto-reconnect cleanly (no retry logic)
# - Some app instances may need restart: docker compose restart payment-api
# - Surviving DB state still consistent: I1, I2, I3 PASS.
# Lesson preview (v2): reconnect logic, exponential backoff, health-driven retry.
```

---

## 13. Getting Started (README excerpt)

```markdown
# Distributed Payment System — v1

## Prerequisites
- Docker + Docker Compose
- Go 1.23+
- `jq` (`brew install jq`)

## Quickstart
```bash
git clone <repo>
cd distributed-payment-system

# Bring up Postgres + payment-api
make up

# Wait ~10s for healthcheck, then verify clean state
make verify

# Generate load
RPS=50 DURATION=30s EXPERIMENT_ID=00 make sim

# Re-verify
make verify

# Tail logs in another terminal
make logs
```

## Reproduce a failure
```bash
bash experiments/v1/01-duplicate-request.md   # or cat and copy-paste
```

## Reset between experiments
```bash
make reset   # nukes DB volume, fresh start
```

## Render HTML reports
```bash
make render-v1   # produces .html for each experiment
open experiments/v1/index.html
```
```

---

## 14. Build & Run Sequence

First-time setup, copy-pasteable:

```bash
git init distributed-payment-system && cd distributed-payment-system
go mod init github.com/yourname/distributed-payment-system
go mod tidy   # after adding code

# Create all files per this spec
# ...

# Boot
make up

# Wait for healthy
sleep 10

# Verify clean state
make verify

# First load test
RPS=20 WORKERS=5 DURATION=30s EXPERIMENT_ID=00 make sim
make verify

# Should see: I1 ✓, I2 ✓, I3 ✓, all balances correct
```

---

## 15. Definition of Done (v1)

See `V1-GRILL-DECISIONS.md` § Q9 for the full 7-section checklist. Summary:

1. All 3 binaries build clean
2. Happy path proven (verify passes after 30s @ 50 RPS, p99 < 100ms)
3. All 5 failure modes reproduced + documented in `experiments/v1/`
4. Structured JSON logs with `request_id` working
5. `docs/v1-spec.md` matches built code; `README.md` < 3-min onboarding
6. `docker compose down -v && up -d` clean reset works
7. `experiments/v1/reflection.md` written

Trigger to v2: all 7 checked → `git tag v1` → `docs/v2-spec.md`.

---

## 16. Out of Scope (for v1 — DO NOT add)

- Idempotency keys
- HTTP retries / backoff
- Outbox / event sourcing
- Authentication / authorization
- Multi-currency
- Refund / reversal endpoints
- Connection pool tuning
- Indexes beyond PK
- Prometheus / Grafana / OpenTelemetry
- Automated unit / integration tests
- Migrations tool (golang-migrate)
- Graceful shutdown
- TLS / mTLS
- Rate limiting
- CI/CD pipelines
- ORM
- gRPC
- Kubernetes manifests
- pgbouncer / connection proxy

Each of these is a lesson for v2–v8. Premature inclusion dilutes the v1 lessons.

---

## 17. Spec ↔ Code Drift Policy

This spec is binding for v1. If implementation diverges, EITHER:
- Update the spec to match (if the divergence is a real improvement), OR
- Fix the code to match (if the divergence is incidental)

Drift between spec and code = bug. Catch it during the "done" checklist review.
