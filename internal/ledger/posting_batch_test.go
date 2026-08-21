package ledger

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestPostgresPostingBatchCommitsMultipleTransactions(t *testing.T) {
	database := openPostgresTestDB(t)
	store := NewPostgresStore(database)
	cash, _ := store.CreateAccount("Batch cash", AssetType)
	revenue, _ := store.CreateAccount("Batch revenue", RevenueType)
	results, err := store.CreateTransactionsBatch([][]EntryRequest{
		{{AccountID: cash.ID, Debit: decimal.NewFromInt(10)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(10)}},
		{{AccountID: cash.ID, Debit: decimal.NewFromInt(20)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(20)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, result := range results {
		if result.Err != nil || result.Transaction == nil || len(result.Transaction.Entries) != 2 {
			t.Fatalf("result %d = %#v", index, result)
		}
		if result.Transaction.LedgerID != DefaultLedgerID || result.Transaction.Timestamp.IsZero() {
			t.Fatalf("result %d missing ledger/timestamp: %#v", index, result.Transaction)
		}
	}
	if results[0].Transaction.ID == results[1].Transaction.ID {
		t.Fatal("batched operations returned the same transaction ID")
	}
	if balance, err := store.GetBalance(cash.ID); err != nil || !balance.Equal(decimal.NewFromInt(30)) {
		t.Fatalf("cash balance=%s err=%v, want 30", balance, err)
	}
	var transactions, events int
	if err := database.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE event_type='transaction_posted'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if transactions != 2 || events != 2 {
		t.Fatalf("transactions=%d events=%d, want 2/2", transactions, events)
	}
}

func TestPostgresPostingBatchRejectsOnlyOperationThatOverdraws(t *testing.T) {
	database := openPostgresTestDB(t)
	store := NewPostgresStore(database)
	cash, expense := seedPostgresBalance(t, store)
	withdrawal := []EntryRequest{{AccountID: expense.ID, Debit: decimal.NewFromInt(60)}, {AccountID: cash.ID, Credit: decimal.NewFromInt(60)}}
	results, err := store.CreateTransactionsBatch([][]EntryRequest{withdrawal, withdrawal})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil || results[0].Transaction == nil {
		t.Fatalf("first withdrawal failed: %#v", results[0])
	}
	if results[1].Err == nil || results[1].Transaction != nil {
		t.Fatalf("second withdrawal was not rejected: %#v", results[1])
	}
	if balance, err := store.GetBalance(cash.ID); err != nil || !balance.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("cash balance=%s err=%v, want 40", balance, err)
	}
}

func TestPostgresPostingBatchInvalidNeighborDoesNotPoisonValidPost(t *testing.T) {
	database := openPostgresTestDB(t)
	store := NewPostgresStore(database)
	cash, _ := store.CreateAccount("Neighbor cash", AssetType)
	revenue, _ := store.CreateAccount("Neighbor revenue", RevenueType)
	results, err := store.CreateTransactionsBatch([][]EntryRequest{
		{{AccountID: uuid.NewString(), Debit: decimal.NewFromInt(5)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(5)}},
		{{AccountID: cash.ID, Debit: decimal.NewFromInt(7)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(7)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == nil || results[0].Transaction != nil {
		t.Fatalf("missing-account operation succeeded: %#v", results[0])
	}
	if results[1].Err != nil || results[1].Transaction == nil {
		t.Fatalf("valid neighbor failed: %#v", results[1])
	}
	if balance, err := store.GetBalance(cash.ID); err != nil || !balance.Equal(decimal.NewFromInt(7)) {
		t.Fatalf("cash balance=%s err=%v, want 7", balance, err)
	}
}

func TestBatchingPostgresStoreReturnsEveryConcurrentResponse(t *testing.T) {
	database := openPostgresTestDB(t)
	store := NewPostgresStore(database)
	batching, err := NewBatchingPostgresStore(store, 16, 2*time.Millisecond, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer batching.Close()
	cash, _ := store.CreateAccount("Concurrent batch cash", AssetType)
	revenue, _ := store.CreateAccount("Concurrent batch revenue", RevenueType)
	entries := []EntryRequest{{AccountID: cash.ID, Debit: decimal.NewFromInt(1)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(1)}}

	const requests = 64
	var group sync.WaitGroup
	errors := make(chan error, requests)
	ids := make(chan string, requests)
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			transaction, existed, err := batching.CreateTransaction("", entries)
			if err == nil && existed {
				err = ErrIdempotencyKeyConflict
			}
			if err == nil {
				ids <- transaction.ID
			}
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	close(ids)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	unique := make(map[string]bool)
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != requests {
		t.Fatalf("unique transaction responses=%d, want %d", len(unique), requests)
	}
	if balance, err := store.GetBalance(cash.ID); err != nil || !balance.Equal(decimal.NewFromInt(requests)) {
		t.Fatalf("cash balance=%s err=%v, want %d", balance, err, requests)
	}
}
