package ledger

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestTransactionRequestChecksum_IsCanonical(t *testing.T) {
	accountA := uuid.NewString()
	accountB := uuid.NewString()
	first := []EntryRequest{
		{AccountID: accountA, Debit: decimal.NewFromInt(10)},
		{AccountID: accountB, Credit: decimal.RequireFromString("10.00")},
	}
	sameLogicalRequest := []EntryRequest{
		{AccountID: accountB, Credit: decimal.NewFromInt(10)},
		{AccountID: accountA, Debit: decimal.RequireFromString("10.0")},
	}

	if got, want := TransactionRequestChecksum("", first), TransactionRequestChecksum("", sameLogicalRequest); got != want {
		t.Fatalf("same logical request produced different checksums: %s and %s", got, want)
	}
	if TransactionRequestChecksum("", first) == TransactionRequestChecksum("", []EntryRequest{
		{AccountID: accountA, Debit: decimal.NewFromInt(11)},
		{AccountID: accountB, Credit: decimal.NewFromInt(11)},
	}) {
		t.Fatal("changed amount produced the same checksum")
	}
	if TransactionRequestChecksum("", first) == TransactionRequestChecksum("", []EntryRequest{
		{AccountID: uuid.NewString(), Debit: decimal.NewFromInt(10)},
		{AccountID: accountB, Credit: decimal.NewFromInt(10)},
	}) {
		t.Fatal("changed account produced the same checksum")
	}
}

func TestMemoryStore_IdempotencyConflictAndConcurrentDuplicates(t *testing.T) {
	store := NewMemoryStore()
	cash, _ := store.CreateAccount("Cash", AssetType)
	revenue, _ := store.CreateAccount("Revenue", RevenueType)
	entries := []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(10)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(10)},
	}

	first, existed, err := store.CreateTransaction("retry-key", entries)
	if err != nil || existed {
		t.Fatalf("first request: existed=%t err=%v", existed, err)
	}
	second, existed, err := store.CreateTransaction("retry-key", []EntryRequest{entries[1], entries[0]})
	if err != nil || !existed || second.ID != first.ID {
		t.Fatalf("duplicate request: transaction=%#v existed=%t err=%v", second, existed, err)
	}
	_, _, err = store.CreateTransaction("retry-key", []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(11)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(11)},
	})
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}

	concurrentStore := NewMemoryStore()
	cash, _ = concurrentStore.CreateAccount("Cash", AssetType)
	revenue, _ = concurrentStore.CreateAccount("Revenue", RevenueType)
	entries = []EntryRequest{{AccountID: cash.ID, Debit: decimal.NewFromInt(10)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(10)}}
	start := make(chan struct{})
	results := make(chan *Transaction, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			transaction, _, err := concurrentStore.CreateTransaction("concurrent-key", entries)
			results <- transaction
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	var transactionID string
	for transaction := range results {
		if transaction != nil && (transactionID == "" || transaction.ID == transactionID) {
			transactionID = transaction.ID
			continue
		}
		t.Fatalf("concurrent retries produced different transactions: %s and %s", transactionID, transaction.ID)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry: %v", err)
		}
	}
	if len(concurrentStore.transactions) != 1 || len(concurrentStore.transactions[0].Entries) != 2 {
		t.Fatalf("expected one transaction with one entry set, got %#v", concurrentStore.transactions)
	}
}
