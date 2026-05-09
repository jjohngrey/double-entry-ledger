package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
)

type CreateAccountRequest struct {
	Name string `json:"name"`
	Type ledger.AccountType `json:"type"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func CreateAccountHandler(store *ledger.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CreateAccountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(ErrorResponse{Error: "Invalid request body"})
			return
		}
		
		account, err := store.CreateAccount(req.Name, req.Type)

		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(ErrorResponse{Error: err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(account)
	}
}
