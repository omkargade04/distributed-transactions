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

// payment-api entrypoint with v2 graceful shutdown and migrations.
//
// TODO (you): wire up the v2 additions.
//
// Required sequence:
//   1. slog default handler (already correct below)
//   2. config.Load
//   3. db.Migrate(cfg.DBURL)        ← NEW v2 — run BEFORE Open
//   4. db.Open(cfg.DBURL)
//   5. api.NewServer(cfg.Port, dbx)
//   6. Run srv.ListenAndServe in a goroutine
//   7. Block on signal channel (SIGINT, SIGTERM)
//   8. On signal: srv.Shutdown(ctx with 30s timeout)
//
// Pitfalls:
//   - ListenAndServe must run in its own goroutine — otherwise main() blocks
//     and you never reach the signal handler.
//   - http.ErrServerClosed is the EXPECTED error after Shutdown — do NOT treat it
//     as a failure. Use errors.Is(err, http.ErrServerClosed) to distinguish.
//   - The Shutdown ctx timeout (30s) must be SHORTER than docker-compose stop_grace_period (35s).
//     Otherwise Docker SIGKILLs you mid-drain.
//   - Migrate runs on its own connection (closes after). Order matters:
//     migrate → open pool. Concurrent migrate + open pool can lock-conflict.
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config.load_failed", "error", err.Error())
		os.Exit(1)
	}

	// TODO: call db.Migrate(cfg.DBURL) here.
	// On error → log "db.migrate_failed" + os.Exit(1).
	// On success → log "db.migrate.complete".

	dbx, err := db.Open(cfg.DBURL)
	if err != nil {
		slog.Error("db.open_failed", "error", err.Error())
		os.Exit(1)
	}
	defer dbx.Close()

	srv := api.NewServer(cfg.Port, dbx)

	// TODO: graceful shutdown wiring
	//
	// sigCh := make(chan os.Signal, 1)
	// signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	//
	// go func() {
	//     slog.Info("server.start", "svc", "payment-api", "port", cfg.Port)
	//     if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
	//         slog.Error("server.listen_failed", "error", err.Error())
	//         os.Exit(1)
	//     }
	// }()
	//
	// <-sigCh
	// slog.Info("server.shutdown.starting")
	//
	// ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// defer cancel()
	// if err := srv.Shutdown(ctx); err != nil {
	//     slog.Error("server.shutdown.error", "error", err.Error())
	//     os.Exit(1)
	// }
	// slog.Info("server.shutdown.complete")

	// PLACEHOLDER — for v1 compatibility while you wire above:
	slog.Info("server.start", "svc", "payment-api", "port", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server.shutdown", "error", err.Error())
		os.Exit(1)
	}

	// suppress unused-import lints while stubs are in place
	_ = context.Background
	_ = signal.Notify
	_ = syscall.SIGTERM
	_ = time.Second
}
