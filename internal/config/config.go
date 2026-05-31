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
func Load() (*Config, error) {
	// 1. DB_URL is required. os.Getenv returns "" when unset — no fallback needed.
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DB_URL required")
	}

	// 2. PORT — default "8080" if unset, then parse.
	//    Use the helper getEnvDefault so we don't pass "" into Atoi.
	portStr := getEnvDefault("PORT", "8080")
	// strconv.Atoi returns TWO values: (int, error). Capture both.
	port, err := strconv.Atoi(portStr)
	if err != nil {
		// Wrap with %w so callers can errors.Is / errors.Unwrap the underlying strconv.NumError.
		return nil, fmt.Errorf("invalid PORT %q: %w", portStr, err)
	}

	// 3. LOG_LEVEL — default "info".
	logLevel := getEnvDefault("LOG_LEVEL", "info")

	return &Config{
		DBURL:    dbURL,
		Port:     port,
		LogLevel: logLevel,
	}, nil
}

// getEnvDefault returns os.Getenv(key) or def if empty.
func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
