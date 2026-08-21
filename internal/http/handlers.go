package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
)

type aggregateReader interface {
	GetAccountDailyAggregates(context.Context, string) (ledger.AggregateResponse, error)
	GetLedgerDailyAggregates(context.Context, string) (ledger.AggregateResponse, error)
}

type transactionProjectionReader interface {
	GetTransactionProjectionStatus(context.Context, string, string) (ledger.TransactionProjectionStatus, error)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ledger.ErrorResponse{Error: msg})
}

func CreateAccountHandler(store ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ledger.CreateAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		var account *ledger.Account
		var err error
		if req.LedgerID != "" {
			ledgerStore, ok := store.(interface {
				CreateAccountInLedger(string, string, ledger.AccountType) (*ledger.Account, error)
			})
			if !ok {
				writeError(w, http.StatusNotImplemented, "ledger-scoped accounts are not supported")
				return
			}
			account, err = ledgerStore.CreateAccountInLedger(req.LedgerID, req.Name, req.Type)
		} else {
			account, err = store.CreateAccount(req.Name, req.Type)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(account)
	}
}

func CreateTransferHandler(store ledger.TransferStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ledger.TransferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if req.IdempotencyKey == "" {
			req.IdempotencyKey = r.Header.Get("Idempotency-Key")
		}
		transfer, existed, err := store.CreateTransfer(req)
		if err != nil {
			if errors.Is(err, ledger.ErrTransferIdempotencyConflict) {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if existed {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
		json.NewEncoder(w).Encode(transfer)
	}
}

func GetTransferHandler(store ledger.TransferStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		transferID := chi.URLParam(r, "transfer_id")
		wait, err := requestedWait(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var transfer *ledger.TransferResponse
		if waiter, ok := store.(interface {
			WaitForTransfer(context.Context, string, time.Duration) (*ledger.TransferResponse, error)
		}); ok && wait > 0 {
			transfer, err = waiter.WaitForTransfer(r.Context(), transferID, wait)
		} else {
			transfer, err = store.GetTransfer(transferID)
		}
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(transfer)
	}
}

func GetAccountEntriesHandler(store ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "account_id")
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "account_id path parameter is required")
			return
		}

		params := ledger.GetAccountEntriesParams{
			To: time.Now(),
		}
		if fromStr := r.URL.Query().Get("from"); fromStr != "" {
			from, err := time.Parse(time.RFC3339, fromStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid 'from' timestamp, expected RFC3339")
				return
			}
			params.From = from
		}
		if toStr := r.URL.Query().Get("to"); toStr != "" {
			to, err := time.Parse(time.RFC3339, toStr)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid 'to' timestamp, expected RFC3339")
				return
			}
			params.To = to
		}

		res, err := store.GetAccountEntries(accountID, params)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}
}

func GetBalanceHandler(store ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := r.URL.Query().Get("account_id")
		if accountID == "" {
			writeError(w, http.StatusBadRequest, "account_id query parameter is required")
			return
		}

		balance, err := store.GetBalance(accountID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ledger.GetBalanceHandlerResponse{Balance: balance})
	}
}

func GetAccountAggregatesHandler(store aggregateReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := store.GetAccountDailyAggregates(r.Context(), chi.URLParam(r, "account_id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func GetLedgerDailyAggregatesHandler(store aggregateReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := store.GetLedgerDailyAggregates(r.Context(), chi.URLParam(r, "ledger_id"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func GetTransactionProjectionStatusHandler(store transactionProjectionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		transactionID := chi.URLParam(r, "transaction_id")
		if _, err := uuid.Parse(transactionID); err != nil {
			writeError(w, http.StatusBadRequest, "transaction_id path parameter must be a UUID")
			return
		}
		wait, err := requestedWait(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var status ledger.TransactionProjectionStatus
		if waiter, ok := store.(interface {
			WaitForTransactionProjection(context.Context, string, string, time.Duration) (ledger.TransactionProjectionStatus, error)
		}); ok && wait > 0 {
			status, err = waiter.WaitForTransactionProjection(r.Context(), transactionID, ledger.AggregateConsumerName, wait)
		} else {
			status, err = store.GetTransactionProjectionStatus(r.Context(), transactionID, ledger.AggregateConsumerName)
		}
		if errors.Is(err, ledger.ErrTransactionProjectionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read transaction projection status")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

func requestedWait(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("wait_ms")
	if raw == "" {
		return 0, nil
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds < 0 || milliseconds > 30000 {
		return 0, errors.New("wait_ms must be an integer from 0 through 30000")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func CreateTransactionHandler(store ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ledger.CreateTransactionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		idempotencyKey := r.Header.Get("Idempotency-Key")
		if idempotencyKey == "" {
			idempotencyKey = req.IdempotencyKey
		}
		transaction, existed, err := store.CreateTransaction(idempotencyKey, req.Entries)
		if err != nil {
			if errors.Is(err, ledger.ErrIdempotencyKeyConflict) {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			if errors.Is(err, ledger.ErrTransactionRetryExhausted) {
				writeError(w, http.StatusServiceUnavailable, ledger.ErrTransactionRetryExhausted.Error())
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		statusCode := http.StatusCreated
		if existed {
			statusCode = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(transaction)
	}
}
