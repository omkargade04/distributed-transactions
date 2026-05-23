package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// ctxKey is an unexported type used as context.Context key.
// Using a custom type (not string) prevents collisions with other packages' keys.
type ctxKey string

const requestIDKey ctxKey = "request_id"

// RequestIDMiddleware extracts X-Request-ID from incoming requests or generates a new UUID,
// then stores it in the request context.
//
// TODO (you): implement.
//
// Requirements:
//   1. Read r.Header.Get("X-Request-ID"). If empty, generate one with uuid.NewString().
//   2. Echo it back in the response header: w.Header().Set("X-Request-ID", reqID)
//   3. Store it in the request context: context.WithValue(r.Context(), requestIDKey, reqID)
//   4. Call next.ServeHTTP(w, r.WithContext(ctx))
//
// Why context, not a global? Context flows naturally through call chains. Goroutine-safe.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: implement (delete the line below once you call uuid.NewString())
		_ = uuid.NewString
		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware logs request.received and request.completed events.
//
// TODO (you): implement.
//
// Requirements:
//   1. Capture start := time.Now()
//   2. Read request_id from context (use RequestIDFromContext helper below)
//   3. slog.InfoContext("request.received", "request_id", reqID, "method", r.Method, "path", r.URL.Path)
//   4. Wrap w in a statusWriter so we can record the status code (see struct below)
//   5. Call next.ServeHTTP(sw, r)
//   6. slog.InfoContext("request.completed", with status + duration_ms)
//
// Why wrap ResponseWriter? Standard http.ResponseWriter does not expose the status code
// after WriteHeader. The wrapper captures it.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: implement
		next.ServeHTTP(w, r)
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader is called by handlers (or implicitly on first Write).
// We intercept to record the code.
func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// RequestIDFromContext reads the request_id stashed by RequestIDMiddleware.
// Returns "" if not present.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}
