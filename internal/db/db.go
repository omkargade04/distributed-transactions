package db

import (
	"database/sql"
	"fmt"

	// Side-effect import: registers the pgx driver with database/sql.
	// We can then call sql.Open("pgx", ...) without referring to the pgx package directly.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open creates a *sql.DB connection pool and verifies connectivity via Ping.
//
// TODO (you): implement this function.
//
// Requirements:
//   1. Call sql.Open("pgx", dbURL). This does NOT actually connect — it just configures the pool.
//   2. Call db.Ping() to force a real connection and surface errors early.
//   3. If either step errors, wrap with fmt.Errorf("sql.Open: %w", err) or similar, and return.
//   4. Return the *sql.DB.
//
// What we deliberately DO NOT do in v1:
//   - No SetMaxOpenConns / SetMaxIdleConns / SetConnMaxLifetime.
//   - Defaults: MaxOpenConns=0 (unlimited), MaxIdleConns=2.
//   - This will cause pool exhaustion under load → failure mode #4. That's the lesson.
//
// Hint:
//   - *sql.DB is safe for concurrent use. Don't open one per request.
//   - The caller (main) is responsible for db.Close() on shutdown.
func Open(dbURL string) (*sql.DB, error) {
	// TODO: implement
	return nil, fmt.Errorf("Open not implemented")
}
