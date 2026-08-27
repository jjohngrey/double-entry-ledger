package ledger

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type postgresTransferFixture struct {
	store       *PostgresStore
	request     TransferRequest
	source      *Account
	destination *Account
}

func newPostgresTransferFixture(t *testing.T) postgresTransferFixture {
	t.Helper()

	database := openPostgresTestDB(t)
	store := NewPostgresStore(database)
	sourceLedgerID := uuid.NewString()
	destinationLedgerID := uuid.NewString()

	source, err := store.CreateAccountInLedger(sourceLedgerID, "Source cash", AssetType)
	if err != nil {
		t.Fatalf("create source account: %v", err)
	}
	sourceOffset, err := store.CreateAccountInLedger(sourceLedgerID, "Source opening balance", RevenueType)
	if err != nil {
		t.Fatalf("create source offset account: %v", err)
	}
	destination, err := store.CreateAccountInLedger(destinationLedgerID, "Destination cash", AssetType)
	if err != nil {
		t.Fatalf("create destination account: %v", err)
	}

	if _, _, err := store.CreateTransaction("postgres-transfer-funding", []EntryRequest{
		{AccountID: source.ID, Debit: decimal.NewFromInt(100)},
		{AccountID: sourceOffset.ID, Credit: decimal.NewFromInt(100)},
	}); err != nil {
		t.Fatalf("fund source account: %v", err)
	}

	return postgresTransferFixture{
		store:       store,
		source:      source,
		destination: destination,
		request: TransferRequest{
			SourceLedgerID:       sourceLedgerID,
			SourceAccountID:      source.ID,
			DestinationLedgerID:  destinationLedgerID,
			DestinationAccountID: destination.ID,
			Amount:               decimal.NewFromInt(10),
			IdempotencyKey:       "postgres-transfer-1",
		},
	}
}

func TestPostgresTransfer_CompletesWithExactBalances(t *testing.T) {
	fixture := newPostgresTransferFixture(t)

	created, existed, err := fixture.store.CreateTransfer(fixture.request)
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if existed {
		t.Fatal("new transfer was reported as existing")
	}
	if created.Status != TransferPending {
		t.Fatalf("created transfer status = %s, want %s", created.Status, TransferPending)
	}

	processed, err := fixture.store.ProcessOutbox(10)
	if err != nil {
		t.Fatalf("process transfer outbox: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed outbox events = %d, want 1", processed)
	}

	completed, err := fixture.store.GetTransfer(created.ID)
	if err != nil {
		t.Fatalf("get completed transfer: %v", err)
	}
	if completed.Status != TransferCompleted {
		t.Fatalf("completed transfer status = %s, want %s", completed.Status, TransferCompleted)
	}
	assertPostgresTransferBalance(t, fixture.store, fixture.source.ID, decimal.NewFromInt(90))
	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.NewFromInt(10))
}

func TestPostgresTransfer_SameKeyDoesNotDoubleDebit(t *testing.T) {
	fixture := newPostgresTransferFixture(t)

	first, existed, err := fixture.store.CreateTransfer(fixture.request)
	if err != nil {
		t.Fatalf("create first transfer: %v", err)
	}
	if existed {
		t.Fatal("first transfer was reported as existing")
	}
	second, existed, err := fixture.store.CreateTransfer(fixture.request)
	if err != nil {
		t.Fatalf("create duplicate transfer: %v", err)
	}
	if !existed {
		t.Fatal("duplicate transfer was reported as new")
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate transfer ID = %s, want %s", second.ID, first.ID)
	}

	assertPostgresTransferBalance(t, fixture.store, fixture.source.ID, decimal.NewFromInt(90))

	var sagaCount, destinationEventCount int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM sagas WHERE idempotency_key=$1`, fixture.request.IdempotencyKey).Scan(&sagaCount); err != nil {
		t.Fatalf("count transfer sagas: %v", err)
	}
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE saga_id=$1 AND event_type='destination_credit'`, uuid.MustParse(first.ID)).Scan(&destinationEventCount); err != nil {
		t.Fatalf("count destination events: %v", err)
	}
	if sagaCount != 1 || destinationEventCount != 1 {
		t.Fatalf("duplicate persisted saga_count=%d destination_event_count=%d, want 1 and 1", sagaCount, destinationEventCount)
	}
}

func TestPostgresTransfer_ChangedRequestConflicts(t *testing.T) {
	fixture := newPostgresTransferFixture(t)

	if _, existed, err := fixture.store.CreateTransfer(fixture.request); err != nil || existed {
		t.Fatalf("create first transfer: existed=%t err=%v", existed, err)
	}
	changed := fixture.request
	changed.Amount = decimal.NewFromInt(11)
	if _, _, err := fixture.store.CreateTransfer(changed); !errors.Is(err, ErrTransferIdempotencyConflict) {
		t.Fatalf("changed request error = %v, want %v", err, ErrTransferIdempotencyConflict)
	}

	assertPostgresTransferBalance(t, fixture.store, fixture.source.ID, decimal.NewFromInt(90))
	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.Zero)
	var sagaCount int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM sagas WHERE idempotency_key=$1`, fixture.request.IdempotencyKey).Scan(&sagaCount); err != nil {
		t.Fatalf("count transfer sagas: %v", err)
	}
	if sagaCount != 1 {
		t.Fatalf("saga count = %d, want 1", sagaCount)
	}
}

func TestPostgresTransfer_RejectsAccountLedgerBoundaryMismatch(t *testing.T) {
	fixture := newPostgresTransferFixture(t)

	tests := []struct {
		name           string
		idempotencyKey string
		wantMessage    string
		modify         func(*TransferRequest)
	}{
		{
			name:           "source account",
			idempotencyKey: "postgres-boundary-source",
			wantMessage:    "source: account does not belong to ledger",
			modify: func(request *TransferRequest) {
				request.SourceAccountID = fixture.destination.ID
			},
		},
		{
			name:           "destination account",
			idempotencyKey: "postgres-boundary-destination",
			wantMessage:    "destination: account does not belong to ledger",
			modify: func(request *TransferRequest) {
				request.DestinationAccountID = fixture.source.ID
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := fixture.request
			request.IdempotencyKey = test.idempotencyKey
			test.modify(&request)
			if _, _, err := fixture.store.CreateTransfer(request); err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("boundary error = %v, want message %q", err, test.wantMessage)
			}
		})
	}

	assertPostgresTransferBalance(t, fixture.store, fixture.source.ID, decimal.NewFromInt(100))
	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.Zero)
	var sagaCount int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM sagas`).Scan(&sagaCount); err != nil {
		t.Fatalf("count transfer sagas: %v", err)
	}
	if sagaCount != 0 {
		t.Fatalf("saga count after rejected transfers = %d, want 0", sagaCount)
	}
}

func TestPostgresTransfer_ReplayedOutboxDoesNotDoubleCredit(t *testing.T) {
	fixture := newPostgresTransferFixture(t)

	created, _, err := fixture.store.CreateTransfer(fixture.request)
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	processed, err := fixture.store.ProcessOutbox(10)
	if err != nil {
		t.Fatalf("process first outbox pass: %v", err)
	}
	if processed != 1 {
		t.Fatalf("first outbox pass processed %d events, want 1", processed)
	}
	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.NewFromInt(10))

	var transactionCountBefore int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&transactionCountBefore); err != nil {
		t.Fatalf("count transactions before replay: %v", err)
	}
	processed, err = fixture.store.ProcessOutbox(10)
	if err != nil {
		t.Fatalf("process replayed outbox pass: %v", err)
	}
	if processed != 0 {
		t.Fatalf("replayed outbox pass processed %d events, want 0", processed)
	}

	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.NewFromInt(10))
	completed, err := fixture.store.GetTransfer(created.ID)
	if err != nil {
		t.Fatalf("get transfer after replay: %v", err)
	}
	if completed.Status != TransferCompleted {
		t.Fatalf("transfer status after replay = %s, want %s", completed.Status, TransferCompleted)
	}
	var transactionCountAfter int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&transactionCountAfter); err != nil {
		t.Fatalf("count transactions after replay: %v", err)
	}
	if transactionCountAfter != transactionCountBefore {
		t.Fatalf("transaction count after replay = %d, want %d", transactionCountAfter, transactionCountBefore)
	}
}

func TestPostgresTransfer_BatchesDestinationCreditsAcrossSagas(t *testing.T) {
	fixture := newPostgresTransferFixture(t)
	const transfers = 8
	created := make([]*TransferResponse, 0, transfers)
	for index := 0; index < transfers; index++ {
		request := fixture.request
		request.Amount = decimal.NewFromInt(5)
		request.IdempotencyKey = fmt.Sprintf("postgres-transfer-batch-%d", index)
		transfer, existed, err := fixture.store.CreateTransfer(request)
		if err != nil || existed {
			t.Fatalf("create transfer %d: existed=%t err=%v", index, existed, err)
		}
		created = append(created, transfer)
	}

	var transactionsBefore int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&transactionsBefore); err != nil {
		t.Fatal(err)
	}
	processed, err := fixture.store.ProcessOutbox(transfers)
	if err != nil {
		t.Fatalf("process destination batch: %v", err)
	}
	if processed != transfers {
		t.Fatalf("processed destination events=%d, want %d", processed, transfers)
	}
	assertPostgresTransferBalance(t, fixture.store, fixture.source.ID, decimal.NewFromInt(60))
	assertPostgresTransferBalance(t, fixture.store, fixture.destination.ID, decimal.NewFromInt(40))
	for _, transfer := range created {
		result, err := fixture.store.GetTransfer(transfer.ID)
		if err != nil || result.Status != TransferCompleted {
			t.Fatalf("transfer %s status=%v err=%v", transfer.ID, result, err)
		}
	}
	var transactionsAfter, completedSteps, processedEvents int
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM transactions`).Scan(&transactionsAfter); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM saga_steps WHERE step='destination_credit' AND status='completed'`).Scan(&completedSteps); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE event_type='destination_credit' AND status='processed'`).Scan(&processedEvents); err != nil {
		t.Fatal(err)
	}
	if transactionsAfter-transactionsBefore != transfers || completedSteps != transfers || processedEvents != transfers {
		t.Fatalf("transactions added=%d completed steps=%d processed events=%d, want %d each", transactionsAfter-transactionsBefore, completedSteps, processedEvents, transfers)
	}
	if replayed, err := fixture.store.ProcessOutbox(transfers); err != nil || replayed != 0 {
		t.Fatalf("replayed destination batch=%d err=%v, want no-op", replayed, err)
	}
}

func assertPostgresTransferBalance(t *testing.T, store *PostgresStore, accountID string, want decimal.Decimal) {
	t.Helper()

	got, err := store.GetBalance(accountID)
	if err != nil {
		t.Fatalf("get balance for account %s: %v", accountID, err)
	}
	if !got.Equal(want) {
		t.Fatalf("balance for account %s = %s, want %s", accountID, got, want)
	}
}
