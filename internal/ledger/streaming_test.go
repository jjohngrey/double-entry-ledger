package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type recordingEventPublisher struct {
	mu    sync.Mutex
	calls map[string]int
}

func (p *recordingEventPublisher) Publish(_ context.Context, _ string, _ []byte, eventID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[eventID]++
	return nil
}

func TestAggregateReplayMatchesRawEntriesAndIsIdempotent(t *testing.T) {
	db := openPostgresTestDB(t)
	store := NewPostgresStore(db)
	cash, err := store.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := store.CreateAccount("Revenue", RevenueType)
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range []int64{10, 25} {
		if _, _, err := store.CreateTransaction("", []EntryRequest{{AccountID: cash.ID, Debit: decimal.NewFromInt(amount)}, {AccountID: revenue.ID, Credit: decimal.NewFromInt(amount)}}); err != nil {
			t.Fatal(err)
		}
	}
	events := drainTransactionEvents(t, store)
	for _, event := range events {
		if applied, err := store.ApplyTransactionPosted(context.Background(), AggregateConsumerName, event); err != nil || !applied {
			t.Fatalf("apply event: applied=%v err=%v", applied, err)
		}
	}
	// A replayed broker delivery must be a no-op.
	if applied, err := store.ApplyTransactionPosted(context.Background(), AggregateConsumerName, events[0]); err != nil || applied {
		t.Fatalf("duplicate applied=%v err=%v", applied, err)
	}

	aggregates, err := store.GetAccountDailyAggregates(context.Background(), cash.ID)
	if err != nil || len(aggregates.Aggregates) != 1 {
		t.Fatalf("get aggregates: %#v err=%v", aggregates, err)
	}
	if got := aggregates.Aggregates[0].Debit; !got.Equal(decimal.NewFromInt(35)) {
		t.Fatalf("debit=%s, want 35", got)
	}
	if aggregates.ProjectionTimestamp.IsZero() || aggregates.LastEventID == "" {
		t.Fatal("projection freshness cursor is missing")
	}
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(SUM(debit),0)::text FROM entries WHERE account_id=$1`, cash.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	rawDebit, err := decimal.NewFromString(raw)
	if err != nil {
		t.Fatalf("parse raw debit %q: %v", raw, err)
	}
	if got := aggregates.Aggregates[0].Debit; !got.Equal(rawDebit) {
		t.Fatalf("aggregate=%s raw=%s", got, rawDebit)
	}
	// A cleared projection can be rebuilt from every retained event.
	if _, err := db.Exec(`DELETE FROM processed_events; DELETE FROM daily_account_aggregates; DELETE FROM daily_ledger_aggregates`); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if applied, err := store.ApplyTransactionPosted(context.Background(), "replay", event); err != nil || !applied {
			t.Fatalf("replay: applied=%v err=%v", applied, err)
		}
	}
	rebuilt, err := store.GetAccountDailyAggregates(context.Background(), cash.ID)
	if err != nil || !rebuilt.Aggregates[0].Debit.Equal(rawDebit) {
		t.Fatalf("rebuilt aggregate: %#v raw=%s err=%v", rebuilt, raw, err)
	}
}

func TestAggregateConsumerRestartRecoversFromProcessedInbox(t *testing.T) {
	db := openPostgresTestDB(t)
	store := NewPostgresStore(db)
	cash, _ := store.CreateAccount("Cash", AssetType)
	equity, _ := store.CreateAccount("Equity", EquityType)
	if _, _, err := store.CreateTransaction("", []EntryRequest{{AccountID: cash.ID, Debit: decimal.NewFromInt(7)}, {AccountID: equity.ID, Credit: decimal.NewFromInt(7)}}); err != nil {
		t.Fatal(err)
	}
	event := drainTransactionEvents(t, store)[0]
	if applied, err := store.ApplyTransactionPosted(context.Background(), AggregateConsumerName, event); err != nil || !applied {
		t.Fatalf("first delivery: applied=%v err=%v", applied, err)
	}
	// A fresh store represents a restarted consumer sharing the durable inbox.
	restarted := NewPostgresStore(db)
	if applied, err := restarted.ApplyTransactionPosted(context.Background(), AggregateConsumerName, event); err != nil || applied {
		t.Fatalf("redelivery after restart: applied=%v err=%v", applied, err)
	}
	aggregates, err := restarted.GetAccountDailyAggregates(context.Background(), cash.ID)
	if err != nil || !aggregates.Aggregates[0].Debit.Equal(decimal.NewFromInt(7)) {
		t.Fatalf("aggregate after restart: %#v err=%v", aggregates, err)
	}
}

func TestAggregateBatchAppliesEachEventExactlyOnce(t *testing.T) {
	db := openPostgresTestDB(t)
	store := NewPostgresStore(db)
	cash, _ := store.CreateAccount("Batch Cash", AssetType)
	revenue, _ := store.CreateAccount("Batch Revenue", RevenueType)
	for _, amount := range []int64{4, 6} {
		if _, _, err := store.CreateTransaction("", []EntryRequest{
			{AccountID: cash.ID, Debit: decimal.NewFromInt(amount)},
			{AccountID: revenue.ID, Credit: decimal.NewFromInt(amount)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	events := drainTransactionEvents(t, store)
	applied, err := store.ApplyTransactionPostedBatch(context.Background(), AggregateConsumerName, []TransactionPostedEvent{events[0], events[0], events[1]})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, value := range applied {
		if value {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("applied %d unique events, want 2: %v", count, applied)
	}
	replayed, err := store.ApplyTransactionPostedBatch(context.Background(), AggregateConsumerName, events)
	if err != nil {
		t.Fatal(err)
	}
	if replayed[0] || replayed[1] {
		t.Fatalf("replayed events applied again: %v", replayed)
	}
	aggregates, err := store.GetAccountDailyAggregates(context.Background(), cash.ID)
	if err != nil || len(aggregates.Aggregates) != 1 || !aggregates.Aggregates[0].Debit.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("batched aggregate=%#v err=%v, want debit 10", aggregates, err)
	}
}

func TestConcurrentBatchPublishersClaimEachEventOnce(t *testing.T) {
	db := openPostgresTestDB(t)
	store := NewPostgresStore(db)
	cash, _ := store.CreateAccount("Publisher Cash", AssetType)
	revenue, _ := store.CreateAccount("Publisher Revenue", RevenueType)
	for range 20 {
		if _, _, err := store.CreateTransaction("", []EntryRequest{
			{AccountID: cash.ID, Debit: decimal.NewFromInt(1)},
			{AccountID: revenue.ID, Credit: decimal.NewFromInt(1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	publisher := &recordingEventPublisher{calls: make(map[string]int)}
	var group sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.PublishCommittedEvents(context.Background(), publisher, 5)
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 20 {
		t.Fatalf("published %d unique events, want 20", len(publisher.calls))
	}
	for eventID, count := range publisher.calls {
		if count != 1 {
			t.Fatalf("event %s published %d times", eventID, count)
		}
	}
	var processed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE event_type='transaction_posted' AND status='processed'`).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed != 20 {
		t.Fatalf("processed outbox rows=%d, want 20", processed)
	}
}

func TestTransactionProjectionStatusTracksExactAggregateEvent(t *testing.T) {
	db := openPostgresTestDB(t)
	store := NewPostgresStore(db)
	cash, err := store.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := store.CreateAccount("Revenue", RevenueType)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, err := store.CreateTransaction("", []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(9)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(9)},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, err := store.GetTransactionProjectionStatus(context.Background(), transaction.ID, AggregateConsumerName)
	if err != nil {
		t.Fatalf("get pending projection status: %v", err)
	}
	if status.Projected || status.TransactionID != transaction.ID || status.Consumer != AggregateConsumerName || status.EventID == "" {
		t.Fatalf("unexpected pending projection status: %#v", status)
	}

	// Publication alone must not look like projection completion. The inbox row
	// is the exact marker committed with the aggregate updates.
	event := drainTransactionEvents(t, store)[0]
	published, err := store.GetTransactionProjectionStatus(context.Background(), transaction.ID, AggregateConsumerName)
	if err != nil {
		t.Fatalf("get published projection status: %v", err)
	}
	if published.Projected || published.EventID != event.EventID {
		t.Fatalf("publication incorrectly marked projected: %#v event=%s", published, event.EventID)
	}

	if applied, err := store.ApplyTransactionPosted(context.Background(), AggregateConsumerName, event); err != nil || !applied {
		t.Fatalf("apply transaction event: applied=%t err=%v", applied, err)
	}
	projected, err := store.GetTransactionProjectionStatus(context.Background(), transaction.ID, AggregateConsumerName)
	if err != nil {
		t.Fatalf("get projected status: %v", err)
	}
	if !projected.Projected || projected.EventID != event.EventID {
		t.Fatalf("event was not reported as projected: %#v", projected)
	}

	_, err = store.GetTransactionProjectionStatus(context.Background(), uuid.NewString(), AggregateConsumerName)
	if !errors.Is(err, ErrTransactionProjectionNotFound) {
		t.Fatalf("missing transaction error=%v, want ErrTransactionProjectionNotFound", err)
	}
	if _, err := store.GetTransactionProjectionStatus(context.Background(), "not-a-uuid", AggregateConsumerName); err == nil {
		t.Fatal("malformed transaction ID was accepted")
	}
}

func drainTransactionEvents(t *testing.T, store *PostgresStore) []TransactionPostedEvent {
	t.Helper()
	var events []TransactionPostedEvent
	for {
		event, err := store.nextTransactionOutboxEvent(context.Background())
		if err != nil {
			if len(events) == 0 {
				t.Fatal(err)
			}
			break
		}
		events = append(events, event)
		if _, err := store.db.Exec(`UPDATE outbox_events SET status='processed' WHERE id=$1`, event.EventID); err != nil {
			t.Fatal(err)
		}
	}
	return events
}
