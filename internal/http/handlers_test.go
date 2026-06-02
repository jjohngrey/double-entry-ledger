package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
	"github.com/shopspring/decimal"
)

func TestCreateTransactionHandler_BadJSON(t *testing.T) {
	store := ledger.NewMemoryStore()
	req := httptest.NewRequest(http.MethodPost, "/transactions", bytes.NewBufferString("not json"))
	w := httptest.NewRecorder()
	CreateTransactionHandler(store)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestCreateTransactionHandler_Unbalanced(t *testing.T) {
	store := ledger.NewMemoryStore()
	acc1, _ := store.CreateAccount("Cash", ledger.AssetType)
	acc2, _ := store.CreateAccount("Revenue", ledger.RevenueType)

	body := ledger.CreateTransactionRequest{
		Entries: []ledger.EntryRequest{
			{AccountID: acc1.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: acc2.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(50)},
		},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/transactions", bytes.NewBuffer(b))
	w := httptest.NewRecorder()
	CreateTransactionHandler(store)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestCreateTransactionHandler_Valid(t *testing.T) {
	store := ledger.NewMemoryStore()
	acc1, _ := store.CreateAccount("Cash", ledger.AssetType)
	acc2, _ := store.CreateAccount("Revenue", ledger.RevenueType)

	body := ledger.CreateTransactionRequest{
		Entries: []ledger.EntryRequest{
			{AccountID: acc1.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: acc2.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
		},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/transactions", bytes.NewBuffer(b))
	w := httptest.NewRecorder()
	CreateTransactionHandler(store)(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}
}

func TestGetBalanceHandler_MissingAccountID(t *testing.T) {
	store := ledger.NewMemoryStore()
	req := httptest.NewRequest(http.MethodGet, "/balance", nil)
	w := httptest.NewRecorder()
	GetBalanceHandler(store)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
