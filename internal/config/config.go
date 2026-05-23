package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds runtime configuration loaded from environment variables.
//
// We use env vars (not a config file) because:
//   - 12-factor app convention
//   - Docker/Kubernetes inject easily
//   - No secret files to leak into git
type Config struct {
	DBURL    string // required, e.g. "postgres://payment:payment_dev@localhost:5432/payment?sslmode=disable"
	Port     int    // HTTP listen port, default 8080
	LogLevel string // "debug" | "info" | "warn" | "error", default "info"
}

// Load reads env vars and returns Config or an error if anything is missing/invalid.
//
// TODO (you): implement this function.
//
// Requirements:
//   1. Read DB_URL — if empty, return error "DB_URL required"
//   2. Read PORT — default "8080" if unset. Convert to int. If non-numeric, return error.
//   3. Read LOG_LEVEL — default "info" if unset.
//
// Hint — Go idioms you'll use:
//   - os.Getenv("DB_URL") returns "" if unset
//   - strconv.Atoi(s) → (int, error)
//   - fmt.Errorf("xxx: %w", err) wraps an error preserving the chain
//
// Test by running: PORT=abc go run ./cmd/payment-api  → should error on startup.
func Load() (*Config, error) {
	// TODO: implement
	return nil, fmt.Errorf("Load not implemented")
}

// getEnvDefault returns os.Getenv(key) or def if empty.
// Helper — already written so you can focus on Load().
func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Suppress unused-import lint until you wire strconv up.
var _ = strconv.Atoi
