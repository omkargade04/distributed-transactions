package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/omkargade/distributed-payment-system/internal/ledger"
	"github.com/omkargade/distributed-payment-system/internal/transfers"
)

// Handler holds dependencies that every HTTP handler needs.
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

	key, _, hasKey := IdempotencyFromContext(r.Context())

	slog.InfoContext(r.Context(), "transfer.received",
		"request_id", RequestIDFromContext(r.Context()),
		"idempotency_key", key,
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
		// Mark the transfers row as failed so retries with the same key re-execute.
		if hasKey {
			_ = transfers.MarkFailed(r.Context(), h.DB, key, err.Error())
		}
		h.handleTransferError(w, r, err)
		return
	}

	// Persist the response so future requests with the same key get a cached replay.
	if hasKey {
		txnUUID, _ := uuid.Parse(result.TxnID)
		body, _ := json.Marshal(result)
		if err := transfers.MarkCompleted(r.Context(), h.DB, key, txnUUID, http.StatusOK, body); err != nil {
			slog.ErrorContext(r.Context(), "transfers.mark_completed_failed", "error", err.Error())
		}
		writeJSONRaw(w, http.StatusOK, body)
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
