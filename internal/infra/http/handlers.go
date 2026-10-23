package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	maxBodyBytes         = 1 << 20
)

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type handlers struct {
	d deps
}

// ---------------------------------------------------------------- health

func (h *handlers) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (h *handlers) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := h.d.db.Ping(ctx); err != nil {
		h.d.logger.WarnContext(r.Context(), "readiness check failed", slog.Any("error", err))
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ready"})
}

// ---------------------------------------------------------------- wallets

func (h *handlers) createWallet(w http.ResponseWriter, r *http.Request) {
	var req createWalletRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.PlayerID) == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_input", "playerId is required")
		return
	}
	if req.InitialBalance == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_input", "initialBalance is required")
		return
	}

	created, err := h.d.opener.OpenWallet(r.Context(), req.PlayerID, *req.InitialBalance)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+created.ID())
	writeJSON(w, http.StatusCreated, newWalletResponse(created))
}

func (h *handlers) getWallet(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidParam(w, r, "walletId", "wallet id")
	if !ok {
		return
	}
	found, err := h.d.wallets.GetByID(r.Context(), h.d.db, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newWalletResponse(found))
}

func (h *handlers) listLedger(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidParam(w, r, "walletId", "wallet id")
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_input", "limit must be a positive integer")
			return
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")

	if _, err := h.d.wallets.GetByID(r.Context(), h.d.db, id); err != nil {
		h.fail(w, r, err)
		return
	}
	entries, next, err := h.d.ledgers.ListByWalletCursor(r.Context(), h.d.db, id, cursor, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newLedgerPageResponse(entries, next))
}

func (h *handlers) reconcile(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidParam(w, r, "walletId", "wallet id")
	if !ok {
		return
	}
	report, err := h.d.reconciler.Reconcile(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newReconciliationResponse(report))
}

// ---------------------------------------------------------------- wagering

// processWager applies a provider transaction. The provider is always the one
// in the token; a different providerId in the body is rejected with 403.
//
// Status codes: 201 processed (first time), 200 processed (replay),
// 202 pending reference, 422 rejected, 409 idempotency/duplicate conflicts.
func (h *handlers) processWager(w http.ResponseWriter, r *http.Request) {
	providerID := h.d.providerID(r.Context())
	if providerID == "" {
		writeAPIError(w, http.StatusForbidden, "forbidden", "token does not carry a provider_id")
		return
	}
	key := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader))
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "missing_idempotency_key", idempotencyKeyHeader+" header is required")
		return
	}

	var req processWagerRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ProviderID != "" && req.ProviderID != providerID {
		writeAPIError(w, http.StatusForbidden, "provider_mismatch", "providerId does not match the authenticated provider")
		return
	}
	if req.Amount == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_input", "amount is required")
		return
	}

	out, err := h.d.processor.ProcessWager(r.Context(), usecase.ProcessWagerInput{
		ProviderID:                     providerID,
		WalletID:                       req.WalletID,
		IdempotencyKey:                 key,
		ExternalTransactionID:          req.ExternalTransactionID,
		Kind:                           wagering.Kind(strings.TrimSpace(req.Kind)),
		Amount:                         *req.Amount,
		RoundID:                        req.RoundID,
		GameID:                         req.GameID,
		ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}

	status := http.StatusOK
	switch out.Status {
	case wagering.StatusRejected:
		status = http.StatusUnprocessableEntity
	case wagering.StatusPendingReference:
		status = http.StatusAccepted
	case wagering.StatusProcessed:
		if !out.Replayed {
			status = http.StatusCreated
		}
	}
	writeJSON(w, status, newProcessWagerResponse(out))
}

// getTransaction returns a transaction of the authenticated provider; anything
// else (including other providers' transactions) is a 404.
func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := uuidParam(w, r, "transactionId", "transaction id")
	if !ok {
		return
	}
	found, err := h.d.wagers.GetByID(r.Context(), h.d.db, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	providerID := h.d.providerID(r.Context())
	if providerID == "" || found.ProviderID != providerID {
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	writeJSON(w, http.StatusOK, newTransactionResponse(found))
}

func (h *handlers) getTransactionByExternal(w http.ResponseWriter, r *http.Request) {
	pathProvider := chi.URLParam(r, "providerId")
	externalID := chi.URLParam(r, "externalTransactionId")
	if h.d.providerID(r.Context()) != pathProvider {
		writeAPIError(w, http.StatusForbidden, "forbidden", "providerId does not match the authenticated provider")
		return
	}
	found, err := h.d.wagers.GetByProviderAndExternal(r.Context(), h.d.db, pathProvider, externalID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newTransactionResponse(found))
}

// ---------------------------------------------------------------- helpers

func uuidParam(w http.ResponseWriter, r *http.Request, name, label string) (string, bool) {
	v := chi.URLParam(r, name)
	if !uuidPattern.MatchString(v) {
		writeAPIError(w, http.StatusBadRequest, "invalid_input", label+" must be a UUID")
		return "", false
	}
	return strings.ToLower(v), true
}

// decodeBody strictly decodes a single JSON object (max 1 MiB, no unknown fields).
// It writes the error response itself and reports whether decoding succeeded.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
			return false
		}
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "invalid request body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid_json", "request body must contain a single JSON object")
		return false
	}
	return true
}

// fail maps use case and repository errors to HTTP responses. Unexpected errors
// are logged and answered with a generic 500.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, usecase.ErrInvalidInput), errors.Is(err, postgres.ErrInvalidArgument):
		writeAPIError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, postgres.ErrInvalidCursor):
		writeAPIError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
	case errors.Is(err, usecase.ErrWalletNotFound), errors.Is(err, postgres.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, usecase.ErrWalletAlreadyExists):
		writeAPIError(w, http.StatusConflict, "wallet_already_exists", "the player already has a wallet in this currency")
	case errors.Is(err, usecase.ErrIdempotencyConflict):
		writeAPIError(w, http.StatusConflict, "idempotency_conflict", "the idempotency key was already used with a different payload")
	case errors.Is(err, usecase.ErrDuplicateExternalTransaction):
		writeAPIError(w, http.StatusConflict, "duplicate_external_transaction", "the external transaction id was already registered with another idempotency key")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "the request could not be completed")
	default:
		h.d.logger.ErrorContext(r.Context(), "unexpected error",
			slog.Any("error", err),
			slog.String("request_id", middleware.GetReqID(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path))
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal response", slog.Any("error", err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"internal server error"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
