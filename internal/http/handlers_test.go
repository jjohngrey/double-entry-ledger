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

type exhaustedRetryStore struct{ ledger.Store }

func (s exhaustedRetryStore) CreateTransaction(string, []ledger.EntryRequest) (*ledger.Transaction, bool, error) {
	return nil, false, ledger.ErrTransactionRetryExhausted
}

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

func TestCreateTransactionHandler_IdempotencyHeaderAndConflict(t *testing.T) {
	store := ledger.NewMemoryStore()
	acc1, _ := store.CreateAccount("Cash", ledger.AssetType)
	acc2, _ := store.CreateAccount("Revenue", ledger.RevenueType)
	post := func(amount int64) *httptest.ResponseRecorder {
		body := ledger.CreateTransactionRequest{Entries: []ledger.EntryRequest{
			{AccountID: acc1.ID, Debit: decimal.NewFromInt(amount)},
			{AccountID: acc2.ID, Credit: decimal.NewFromInt(amount)},
		}}
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/transactions", bytes.NewBuffer(payload))
		req.Header.Set("Idempotency-Key", "header-key")
		w := httptest.NewRecorder()
		CreateTransactionHandler(store)(w, req)
		return w
	}

	if w := post(10); w.Code != http.StatusCreated {
		t.Fatalf("first request: expected 201, got %d", w.Code)
	}
	if w := post(10); w.Code != http.StatusOK {
		t.Fatalf("duplicate request: expected 200, got %d", w.Code)
	}
	if w := post(11); w.Code != http.StatusConflict {
		t.Fatalf("changed request: expected 409, got %d", w.Code)
	}
}

func TestCreateTransactionHandler_RetryExhaustionReturnsControlled503(t *testing.T) {
	store := exhaustedRetryStore{Store: ledger.NewMemoryStore()}
	body := ledger.CreateTransactionRequest{}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/transactions", bytes.NewBuffer(b))
	w := httptest.NewRecorder()
	CreateTransactionHandler(store)(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	var response ledger.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error != ledger.ErrTransactionRetryExhausted.Error() {
		t.Fatalf("unexpected controlled error: %q", response.Error)
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
