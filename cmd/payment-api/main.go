package main

import (
	"log/slog"
	"os"

	"github.com/omkargade/distributed-payment-system/internal/api"
	"github.com/omkargade/distributed-payment-system/internal/config"
	"github.com/omkargade/distributed-payment-system/internal/db"
)

// payment-api process entrypoint.
//
// TODO (you): implement.
//
// Requirements:
//   1. Configure default slog to emit JSON to stdout at info level:
//        slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
//   2. cfg, err := config.Load()  — if err, log "config.load_failed" and os.Exit(1).
//   3. dbx, err := db.Open(cfg.DBURL) — if err, log "db.open_failed" and os.Exit(1).
//   4. defer dbx.Close()
//   5. srv := api.NewServer(cfg.Port, dbx)
//   6. slog.Info("server.start", "svc", "payment-api", "port", cfg.Port)
//   7. if err := srv.ListenAndServe(); err != nil → log "server.shutdown" with error, os.Exit(1).
//
// NOTE: no graceful shutdown handler in v1. SIGTERM = abrupt death. v2 lesson.
//
// Pattern observation: every "init error" path does
//   log structured error → os.Exit(1)
// We do NOT panic — panics produce ugly stacks, exit is clean.
func main() {
	// TODO: implement
	_ = slog.Default
	_ = os.Exit
	_ = api.NewServer
	_ = config.Load
	_ = db.Open
}
