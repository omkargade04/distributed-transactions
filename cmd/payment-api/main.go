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

	// Run migrations before opening the pool — separate connection, no lock contention.
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

	// Register signal handler BEFORE starting listener so we don't miss a fast SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Run HTTP server in goroutine — main must not block here or it never reaches <-sigCh.
	go func() {
		slog.Info("server.start", "svc", "payment-api", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// ErrServerClosed is normal after Shutdown — anything else is a real failure.
			slog.Error("server.listen_failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	// Block until SIGTERM or SIGINT.
	<-sigCh
	slog.Info("server.shutdown.starting")

	// Give in-flight handlers 30s to finish. docker-compose stop_grace_period=35s,
	// so Docker won't SIGKILL us before this completes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("server.shutdown.error", "error", err.Error())
		os.Exit(1)
	}
	slog.Info("server.shutdown.complete")
}
