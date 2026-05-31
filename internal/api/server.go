package api

import (
	"database/sql"
	"fmt"
	"net/http"
)

// NewServer wires routes + middleware and returns a configured *http.Server.
//
// TODO (you): implement.
//
// Requirements:
//   1. Create a Handler: h := &Handler{DB: dbx}
//   2. Create a ServeMux: mux := http.NewServeMux()
//   3. Register routes (Go 1.22+ pattern syntax — supports method + path params):
//        mux.HandleFunc("GET /health",                 h.Health)
//        mux.HandleFunc("POST /v1/transfer",           h.Transfer)
//        mux.HandleFunc("GET /v1/accounts/{id}",       h.GetAccount)
//   4. Wrap mux with middleware (inside-out order applied at request time):
//        var handler http.Handler = mux
//        handler = LoggingMiddleware(handler)
//        handler = RequestIDMiddleware(handler)
//      Net effect: RequestID wraps Logging wraps mux. RequestID runs FIRST per request.
//   5. Return &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: handler}
//
// Why middleware order matters:
//   - RequestID must be outermost so Logging can read the request_id from context.
//   - If you swap them, log lines lack request_id.
func NewServer(port int, dbx *sql.DB) *http.Server {
	// TODO: implement
	h := &Handler{DB: dbx}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("POST /v1/transfer", h.Transfer)
	mux.HandleFunc("GET /v1/accounts/{id}", h.GetAccount)

	var handler http.Handler = mux
	handler = LoggingMiddleware(handler)
	handler = RequestIDMiddleware(handler)

	return &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: handler}
}
