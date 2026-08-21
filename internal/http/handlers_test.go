package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
	"github.com/shopspring/decimal"
)

type exhaustedRetryStore struct{ ledger.Store }

func (s exhaustedRetryStore) CreateTransaction(string, []ledger.EntryRequest) (*ledger.Transaction, bool, error) {
	return nil, false, ledger.ErrTransactionRetryExhausted
}

type projectionStatusReaderStub struct {
	status      ledger.TransactionProjectionStatus
	err         error
	transaction string
	consumer    string
	calls       int
}

func (s *projectionStatusReaderStub) GetTransactionProjectionStatus(_ context.Context, transactionID, consumer string) (ledger.TransactionProjectionStatus, error) {
	s.calls++
	s.transaction = transactionID
	s.consumer = consumer
	return s.status, s.err
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

func TestGetTransactionProjectionStatusHandler(t *testing.T) {
	transactionID := uuid.NewString()
	eventID := uuid.NewString()
	store := &projectionStatusReaderStub{status: ledger.TransactionProjectionStatus{
		TransactionID: transactionID,
		EventID:       eventID,
		Consumer:      ledger.AggregateConsumerName,
		Projected:     true,
	}}
	router := chi.NewRouter()
	router.Get("/transactions/{transaction_id}/projection-status", GetTransactionProjectionStatusHandler(store))
	req := httptest.NewRequest(http.MethodGet, "/transactions/"+transactionID+"/projection-status", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response ledger.TransactionProjectionStatus
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response != store.status {
		t.Fatalf("unexpected response: %#v", response)
	}
	if store.calls != 1 || store.transaction != transactionID || store.consumer != ledger.AggregateConsumerName {
		t.Fatalf("unexpected store call: calls=%d transaction=%q consumer=%q", store.calls, store.transaction, store.consumer)
	}
}

func TestGetTransactionProjectionStatusHandlerRejectsMalformedID(t *testing.T) {
	store := &projectionStatusReaderStub{}
	router := chi.NewRouter()
	router.Get("/transactions/{transaction_id}/projection-status", GetTransactionProjectionStatusHandler(store))
	req := httptest.NewRequest(http.MethodGet, "/transactions/not-a-uuid/projection-status", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if store.calls != 0 {
		t.Fatalf("malformed ID reached the store %d times", store.calls)
	}
}

func TestGetTransactionProjectionStatusHandlerReturnsNotFound(t *testing.T) {
	store := &projectionStatusReaderStub{err: errors.Join(ledger.ErrTransactionProjectionNotFound, errors.New("missing transaction"))}
	router := chi.NewRouter()
	router.Get("/transactions/{transaction_id}/projection-status", GetTransactionProjectionStatusHandler(store))
	req := httptest.NewRequest(http.MethodGet, "/transactions/"+uuid.NewString()+"/projection-status", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
