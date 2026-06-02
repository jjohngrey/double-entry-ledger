package http

import (
	"encoding/json"
	"net/http"

	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
)

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

		account, err := store.CreateAccount(req.Name, req.Type)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(account)
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
		json.NewEncoder(w).Encode(ledger.BalanceResponse{Balance: balance})
	}
}

func CreateTransactionHandler(store ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ledger.CreateTransactionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		 transaction, existed, err := store.CreateTransaction(req.IdempotencyKey, req.Entries)
		if err != nil {
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
