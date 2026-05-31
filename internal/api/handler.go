package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/omkargade/distributed-payment-system/internal/ledger"
)

// Handler holds dependencies that every HTTP handler needs.
type Handler struct {
	DB *sql.DB
}

// transferRequest is the JSON shape the API accepts on POST /v1/transfer.
type transferRequest struct {
	PayerID     string `json:"payer_id"`
	PayeeID     string `json:"payee_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// errorResponse is the JSON shape returned on any 4xx/5xx.
type errorResponse struct {
	Error string `json:"error"`
}

// Health returns 200 ok — used by Docker healthcheck and load balancers.
//
// TODO (you): implement.
// Requirements:
//   - Set Content-Type: application/json
//   - Write 200 status
//   - Body: {"status":"ok"}
// Hint: use writeJSON helper below.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Transfer handles POST /v1/transfer.
//
// TODO (you): implement.
//
// Requirements:
//   1. Decode JSON request body into transferRequest. If decode fails → 400 {"error":"invalid_json"}.
//   2. Log "transfer.received" with request_id, payer_id, payee_id, amount_minor.
//   3. Call ledger.Transfer(r.Context(), h.DB, ledger.TransferRequest{...}).
//   4. If error, call h.handleTransferError(w, r, err).
//   5. On success → 200 with the TransferResult JSON-encoded.
//
// Hint:
//   - json.NewDecoder(r.Body).Decode(&req) is idiomatic.
//   - Use writeJSON helper (below).
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

// handleTransferError maps domain errors → HTTP status codes + JSON response.
//
// TODO (you): implement.
//
// Map errors with errors.Is():
//   - ledger.ErrAccountNotFound   → 404 "account_not_found"
//   - ledger.ErrInsufficientFunds → 400 "insufficient_funds"  (log transfer.rejected at info)
//   - ledger.ErrSamePayerPayee    → 400 "same_payer_payee"
//   - ledger.ErrInvalidAmount     → 400 "invalid_amount"
//   - anything else               → 500 "internal" (log transfer.failed at error)
//
// Why a separate function? Keeps Transfer handler tidy. Many error types, one mapping.
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

// GetAccount handles GET /v1/accounts/{id}.
//
// TODO (you): implement.
//
// Requirements:
//   1. Read path parameter: id := r.PathValue("id")
//   2. Call ledger.GetAccount(r.Context(), h.DB, id).
//   3. errors.Is(err, ledger.ErrAccountNotFound) → 404.
//   4. Other err → 500 with log "account.lookup_failed".
//   5. On success → 200 with {"id":..., "balance_minor":..., "currency":...}.
//
// Note: r.PathValue("id") is a Go 1.22+ feature. Works with the new net/http patterns
// like "GET /v1/accounts/{id}" which we register in server.go.
func (h *Handler) GetAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	account, err := ledger.GetAccount(r.Context(), h.DB, id)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "account_not_found"})
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "account.lookup_failed", "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal"})
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// writeJSON writes a JSON response with the given status code.
// Helper — already written so you can focus on handlers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
