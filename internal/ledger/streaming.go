package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/shopspring/decimal"
)

const (
	TransactionPostedSubject = "ledger.transaction_posted"
	LedgerStreamName         = "LEDGER"
	AggregateConsumerName    = "daily-aggregates-v1"
)

// ErrTransactionProjectionNotFound means no transaction-posted outbox event
// exists for the requested transaction.
var ErrTransactionProjectionNotFound = errors.New("transaction projection status not found")

var backgroundTxOptions = &sql.TxOptions{Isolation: sql.LevelReadCommitted}

// EventPublisher permits publication testing without a running broker.
type EventPublisher interface {
	Publish(context.Context, string, []byte, string) error
}

type EventPublication struct {
	Subject string
	Data    []byte
	EventID string
}

// BatchEventPublisher submits a bounded group before awaiting acknowledgements,
// overlapping broker latency while preserving an error result per event.
type BatchEventPublisher interface {
	PublishBatch(context.Context, []EventPublication) []error
}

type JetStreamPublisher struct{ js nats.JetStreamContext }

func (p *JetStreamPublisher) JetStream() nats.JetStreamContext { return p.js }

func NewJetStreamPublisher(url string, asyncMaxPending ...int) (*nats.Conn, *JetStreamPublisher, error) {
	maxPending := 256
	if len(asyncMaxPending) > 0 {
		maxPending = asyncMaxPending[0]
	}
	if maxPending <= 0 {
		return nil, nil, errors.New("NATS async max pending must be positive")
	}
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, nil, err
	}
	js, err := nc.JetStream(nats.PublishAsyncMaxPending(maxPending))
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	if _, err := js.StreamInfo(LedgerStreamName); errors.Is(err, nats.ErrStreamNotFound) {
		_, err = js.AddStream(&nats.StreamConfig{Name: LedgerStreamName, Subjects: []string{TransactionPostedSubject}, Storage: nats.FileStorage, Retention: nats.LimitsPolicy})
	}
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("ensure JetStream stream: %w", err)
	}
	return nc, &JetStreamPublisher{js: js}, nil
}

func (p *JetStreamPublisher) Publish(ctx context.Context, subject string, data []byte, eventID string) error {
	// Do not broker-deduplicate by event ID. A publish acknowledgement can be
	// lost after the broker stores the message; re-publishing then intentionally
	// creates an at-least-once duplicate for the inbox to absorb.
	_ = eventID
	_, err := p.js.Publish(subject, data, nats.Context(ctx))
	return err
}

func (p *JetStreamPublisher) PublishBatch(ctx context.Context, publications []EventPublication) []error {
	errorsByIndex := make([]error, len(publications))
	type pendingAck struct {
		index  int
		future nats.PubAckFuture
	}
	pending := make([]pendingAck, 0, len(publications))
	for index, publication := range publications {
		future, err := p.js.PublishAsync(publication.Subject, publication.Data)
		if err != nil {
			errorsByIndex[index] = err
			continue
		}
		pending = append(pending, pendingAck{index: index, future: future})
	}
	for _, acknowledgement := range pending {
		select {
		case <-acknowledgement.future.Ok():
		case err := <-acknowledgement.future.Err():
			errorsByIndex[acknowledgement.index] = err
		case <-ctx.Done():
			errorsByIndex[acknowledgement.index] = ctx.Err()
		}
	}
	return errorsByIndex
}

// PublishCommittedEvents is at-least-once: a crash after Publish but before
// the outbox update can produce a duplicate delivery.
func (s *PostgresStore) PublishCommittedEvents(ctx context.Context, publisher EventPublisher, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	owner := uuid.New()
	claimed, err := s.claimTransactionOutboxEvents(ctx, owner, limit)
	if err != nil || claimed == 0 {
		return 0, err
	}
	events, err := s.hydrateClaimedTransactionEvents(ctx, owner)
	if err != nil {
		_ = s.releaseTransactionOutboxClaim(ctx, owner)
		return 0, err
	}
	publications := make([]EventPublication, len(events))
	publishErrors := make([]error, len(events))
	validPublications := make([]EventPublication, 0, len(events))
	validIndexes := make([]int, 0, len(events))
	for index, event := range events {
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			publishErrors[index] = marshalErr
			continue
		}
		publications[index] = EventPublication{Subject: TransactionPostedSubject, Data: payload, EventID: event.EventID}
		validPublications = append(validPublications, publications[index])
		validIndexes = append(validIndexes, index)
	}
	if batchPublisher, ok := publisher.(BatchEventPublisher); ok {
		batchResults := batchPublisher.PublishBatch(ctx, validPublications)
		if len(batchResults) != len(validPublications) {
			batchResults = make([]error, len(validPublications))
			for index := range batchResults {
				batchResults[index] = errors.New("batch publisher returned an invalid result count")
			}
		}
		for resultIndex, eventIndex := range validIndexes {
			publishErrors[eventIndex] = batchResults[resultIndex]
		}
	} else {
		for index, publication := range publications {
			if publishErrors[index] == nil {
				publishErrors[index] = publisher.Publish(ctx, publication.Subject, publication.Data, publication.EventID)
			}
		}
	}
	succeeded := make([]uuid.UUID, 0, len(events))
	failed := make([]uuid.UUID, 0)
	var firstErr error
	for index, event := range events {
		publishErr := publishErrors[index]
		eventID := uuid.MustParse(event.EventID)
		if publishErr != nil {
			failed = append(failed, eventID)
			if firstErr == nil {
				firstErr = publishErr
			}
			continue
		}
		succeeded = append(succeeded, eventID)
	}
	if err := s.completeTransactionOutboxClaim(ctx, owner, succeeded, failed); err != nil {
		return 0, err
	}
	if firstErr != nil {
		return len(succeeded), fmt.Errorf("publish transaction event: %w", firstErr)
	}
	return len(succeeded), nil
}

func (s *PostgresStore) claimTransactionOutboxEvents(ctx context.Context, owner uuid.UUID, limit int) (int, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox_events SET status='pending',lease_owner=NULL,lease_expires_at=NULL WHERE status='processing' AND event_type='transaction_posted' AND lease_expires_at <= NOW()`); err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id FROM outbox_events
			WHERE status='pending' AND event_type='transaction_posted' AND available_at <= NOW()
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT $1
		)
		UPDATE outbox_events AS event
		SET status='processing',lease_owner=$2,lease_expires_at=NOW()+INTERVAL '10 seconds'
		FROM candidates WHERE event.id=candidates.id RETURNING event.id`, limit, owner)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}

func (s *PostgresStore) hydrateClaimedTransactionEvents(ctx context.Context, owner uuid.UUID) ([]TransactionPostedEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id,t.id,t.ledger_id,t.created_at,e.id,e.account_id,e.credit::text,e.debit::text
		FROM outbox_events o
		JOIN transactions t ON t.id=o.transaction_id
		JOIN entries e ON e.transaction_id=t.id
		WHERE o.status='processing' AND o.lease_owner=$1
		ORDER BY o.created_at,o.id,e.id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]TransactionPostedEvent, 0)
	var currentID uuid.UUID
	for rows.Next() {
		var eventID, transactionID, ledgerID, entryID, accountID uuid.UUID
		var timestamp time.Time
		var credit, debit string
		if err := rows.Scan(&eventID, &transactionID, &ledgerID, &timestamp, &entryID, &accountID, &credit, &debit); err != nil {
			return nil, err
		}
		if len(events) == 0 || eventID != currentID {
			currentID = eventID
			events = append(events, TransactionPostedEvent{EventID: eventID.String(), Type: "transaction_posted", TransactionID: transactionID.String(), LedgerID: ledgerID.String(), Timestamp: timestamp})
		}
		parsedCredit, err := decimal.NewFromString(credit)
		if err != nil {
			return nil, err
		}
		parsedDebit, err := decimal.NewFromString(debit)
		if err != nil {
			return nil, err
		}
		index := len(events) - 1
		events[index].Entries = append(events[index].Entries, Entry{ID: entryID.String(), AccountID: accountID.String(), TransactionID: transactionID.String(), Credit: parsedCredit, Debit: parsedDebit})
	}
	return events, rows.Err()
}

func (s *PostgresStore) completeTransactionOutboxClaim(ctx context.Context, owner uuid.UUID, succeeded, failed []uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		WITH completed AS (
			UPDATE outbox_events SET status='processed',processed_at=NOW(),attempt_count=attempt_count+1,lease_owner=NULL,lease_expires_at=NULL
			WHERE lease_owner=$1 AND id=ANY($2::uuid[]) RETURNING id
		)
		UPDATE outbox_events SET status='pending',attempt_count=attempt_count+1,
			available_at=NOW()+(INTERVAL '100 milliseconds'*POWER(2,LEAST(attempt_count,8))),lease_owner=NULL,lease_expires_at=NULL
		WHERE lease_owner=$1 AND id=ANY($3::uuid[])`, owner, succeeded, failed)
	return err
}

func (s *PostgresStore) releaseTransactionOutboxClaim(ctx context.Context, owner uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox_events SET status='pending',lease_owner=NULL,lease_expires_at=NULL WHERE lease_owner=$1`, owner)
	return err
}

func (s *PostgresStore) nextTransactionOutboxEvent(ctx context.Context) (TransactionPostedEvent, error) {
	tx, err := s.db.BeginTx(ctx, backgroundTxOptions)
	if err != nil {
		return TransactionPostedEvent{}, err
	}
	defer tx.Rollback()
	var event TransactionPostedEvent
	var eventID, txID, ledgerID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT o.id,t.id,t.ledger_id,t.created_at FROM outbox_events o JOIN transactions t ON t.id=o.transaction_id WHERE o.status='pending' AND o.event_type='transaction_posted' AND o.available_at <= NOW() ORDER BY o.created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&eventID, &txID, &ledgerID, &event.Timestamp)
	if err != nil {
		return TransactionPostedEvent{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,account_id,credit,debit FROM entries WHERE transaction_id=$1 ORDER BY id`, txID)
	if err != nil {
		return TransactionPostedEvent{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, accountID uuid.UUID
		var credit, debit string
		if err := rows.Scan(&id, &accountID, &credit, &debit); err != nil {
			return TransactionPostedEvent{}, err
		}
		c, err := decimal.NewFromString(credit)
		if err != nil {
			return TransactionPostedEvent{}, err
		}
		d, err := decimal.NewFromString(debit)
		if err != nil {
			return TransactionPostedEvent{}, err
		}
		event.Entries = append(event.Entries, Entry{ID: id.String(), AccountID: accountID.String(), TransactionID: txID.String(), Credit: c, Debit: d})
	}
	if err := rows.Err(); err != nil {
		return TransactionPostedEvent{}, err
	}
	event.EventID, event.Type, event.TransactionID, event.LedgerID = eventID.String(), "transaction_posted", txID.String(), ledgerID.String()
	if err := tx.Commit(); err != nil {
		return TransactionPostedEvent{}, err
	}
	return event, nil
}

func (s *PostgresStore) rescheduleOutboxEvent(ctx context.Context, eventID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox_events SET attempt_count=attempt_count+1, available_at=NOW()+(INTERVAL '100 milliseconds'*POWER(2,LEAST(attempt_count,8))) WHERE id=$1 AND status='pending'`, eventID)
	return err
}

// ApplyTransactionPosted atomically records the inbox row and projection. A
// repeated event therefore does not change totals, even after a restart.
func (s *PostgresStore) ApplyTransactionPosted(ctx context.Context, consumer string, event TransactionPostedEvent) (bool, error) {
	applied, err := s.ApplyTransactionPostedBatch(ctx, consumer, []TransactionPostedEvent{event})
	if err != nil {
		return false, err
	}
	return applied[0], nil
}

type preparedPostedEvent struct {
	index    int
	event    TransactionPostedEvent
	eventID  uuid.UUID
	ledgerID uuid.UUID
}

type aggregateDelta struct {
	id          uuid.UUID
	day         string
	debit       decimal.Decimal
	credit      decimal.Decimal
	lastEventID uuid.UUID
}

// ApplyTransactionPostedBatch amortizes inbox and aggregate writes across a
// short micro-batch while preserving the same atomic inbox/projection commit.
func (s *PostgresStore) ApplyTransactionPostedBatch(ctx context.Context, consumer string, events []TransactionPostedEvent) ([]bool, error) {
	if consumer == "" {
		return nil, errors.New("consumer is required")
	}
	if len(events) == 0 {
		return []bool{}, nil
	}
	prepared := make([]preparedPostedEvent, len(events))
	for index, event := range events {
		eventID, err := uuid.Parse(event.EventID)
		if err != nil {
			return nil, fmt.Errorf("invalid event ID: %w", err)
		}
		ledgerID, err := uuid.Parse(event.LedgerID)
		if err != nil {
			return nil, fmt.Errorf("invalid ledger ID: %w", err)
		}
		if event.Type != "transaction_posted" {
			return nil, fmt.Errorf("unexpected event type %q", event.Type)
		}
		prepared[index] = preparedPostedEvent{index: index, event: event, eventID: eventID, ledgerID: ledgerID}
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].eventID.String() < prepared[j].eventID.String() })

	tx, err := s.db.BeginTx(ctx, backgroundTxOptions)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var inbox strings.Builder
	inbox.WriteString(`INSERT INTO processed_events (consumer,event_id) VALUES `)
	inboxArgs := make([]any, 0, len(prepared)*2)
	for index, event := range prepared {
		if index > 0 {
			inbox.WriteByte(',')
		}
		base := index*2 + 1
		fmt.Fprintf(&inbox, "($%d,$%d)", base, base+1)
		inboxArgs = append(inboxArgs, consumer, event.eventID)
	}
	inbox.WriteString(` ON CONFLICT DO NOTHING RETURNING event_id`)
	rows, err := tx.QueryContext(ctx, inbox.String(), inboxArgs...)
	if err != nil {
		return nil, err
	}
	newEvents := make(map[uuid.UUID]bool, len(prepared))
	for rows.Next() {
		var eventID uuid.UUID
		if err := rows.Scan(&eventID); err != nil {
			rows.Close()
			return nil, err
		}
		newEvents[eventID] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	applied := make([]bool, len(events))
	accountDeltas := make(map[string]aggregateDelta)
	ledgerDeltas := make(map[string]aggregateDelta)
	aggregatedEvents := make(map[uuid.UUID]bool, len(newEvents))
	for _, preparedEvent := range prepared {
		if !newEvents[preparedEvent.eventID] || aggregatedEvents[preparedEvent.eventID] {
			continue
		}
		aggregatedEvents[preparedEvent.eventID] = true
		applied[preparedEvent.index] = true
		day := preparedEvent.event.Timestamp.UTC().Format("2006-01-02")
		ledgerKey := preparedEvent.ledgerID.String() + "|" + day
		ledgerDelta, exists := ledgerDeltas[ledgerKey]
		if !exists {
			ledgerDelta.debit, ledgerDelta.credit = decimal.Zero, decimal.Zero
		}
		ledgerDelta.id, ledgerDelta.day, ledgerDelta.lastEventID = preparedEvent.ledgerID, day, preparedEvent.eventID
		for _, entry := range preparedEvent.event.Entries {
			accountID, err := uuid.Parse(entry.AccountID)
			if err != nil {
				return nil, fmt.Errorf("invalid account ID: %w", err)
			}
			accountKey := accountID.String() + "|" + day
			accountDelta, exists := accountDeltas[accountKey]
			if !exists {
				accountDelta.debit, accountDelta.credit = decimal.Zero, decimal.Zero
			}
			accountDelta.id, accountDelta.day, accountDelta.lastEventID = accountID, day, preparedEvent.eventID
			accountDelta.debit = accountDelta.debit.Add(entry.Debit)
			accountDelta.credit = accountDelta.credit.Add(entry.Credit)
			accountDeltas[accountKey] = accountDelta
			ledgerDelta.debit = ledgerDelta.debit.Add(entry.Debit)
			ledgerDelta.credit = ledgerDelta.credit.Add(entry.Credit)
		}
		ledgerDeltas[ledgerKey] = ledgerDelta
	}
	if err := upsertAggregateDeltas(ctx, tx, "daily_account_aggregates", "account_id", accountDeltas); err != nil {
		return nil, err
	}
	if err := upsertAggregateDeltas(ctx, tx, "daily_ledger_aggregates", "ledger_id", ledgerDeltas); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, event := range events {
		s.projectionWaiters.notify(event.TransactionID)
	}
	return applied, nil
}

func upsertAggregateDeltas(ctx context.Context, tx *sql.Tx, table, idColumn string, deltas map[string]aggregateDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	keys := make([]string, 0, len(deltas))
	for key := range deltas {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var query strings.Builder
	fmt.Fprintf(&query, "INSERT INTO %s (%s,day,debit,credit,last_event_id) VALUES ", table, idColumn)
	args := make([]any, 0, len(keys)*5)
	for index, key := range keys {
		if index > 0 {
			query.WriteByte(',')
		}
		base := index*5 + 1
		fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4)
		delta := deltas[key]
		args = append(args, delta.id, delta.day, delta.debit.String(), delta.credit.String(), delta.lastEventID)
	}
	fmt.Fprintf(&query, ` ON CONFLICT (%s,day) DO UPDATE SET debit=%s.debit+EXCLUDED.debit,credit=%s.credit+EXCLUDED.credit,updated_at=NOW(),last_event_id=EXCLUDED.last_event_id`, idColumn, table, table)
	_, err := tx.ExecContext(ctx, query.String(), args...)
	return err
}

func (s *PostgresStore) GetAccountDailyAggregates(ctx context.Context, accountID string) (AggregateResponse, error) {
	id, err := uuid.Parse(accountID)
	if err != nil {
		return AggregateResponse{}, fmt.Errorf("invalid account ID: %w", err)
	}
	return s.getDailyAggregates(ctx, `SELECT day,debit,credit,updated_at,last_event_id FROM daily_account_aggregates WHERE account_id=$1 ORDER BY day`, id)
}
func (s *PostgresStore) GetLedgerDailyAggregates(ctx context.Context, ledgerID string) (AggregateResponse, error) {
	id, err := uuid.Parse(ledgerID)
	if err != nil {
		return AggregateResponse{}, fmt.Errorf("invalid ledger ID: %w", err)
	}
	return s.getDailyAggregates(ctx, `SELECT day,debit,credit,updated_at,last_event_id FROM daily_ledger_aggregates WHERE ledger_id=$1 ORDER BY day`, id)
}

// GetTransactionProjectionStatus reads the outbox and the consumer inbox in a
// single statement. ApplyTransactionPosted commits its inbox marker and
// aggregate updates atomically, so Projected cannot become true before the
// corresponding projection is durable.
func (s *PostgresStore) GetTransactionProjectionStatus(ctx context.Context, transactionID, consumer string) (TransactionProjectionStatus, error) {
	if consumer == "" {
		return TransactionProjectionStatus{}, errors.New("consumer is required")
	}
	id, err := uuid.Parse(transactionID)
	if err != nil {
		return TransactionProjectionStatus{}, fmt.Errorf("invalid transaction ID: %w", err)
	}

	status := TransactionProjectionStatus{
		TransactionID: id.String(),
		Consumer:      consumer,
	}
	var eventID uuid.UUID
	err = s.db.QueryRowContext(ctx, `
		SELECT o.id,
		       EXISTS (
		           SELECT 1
		           FROM processed_events p
		           WHERE p.consumer = $2 AND p.event_id = o.id
		       )
		FROM outbox_events o
		WHERE o.transaction_id = $1 AND o.event_type = 'transaction_posted'
	`, id, consumer).Scan(&eventID, &status.Projected)
	if errors.Is(err, sql.ErrNoRows) {
		return TransactionProjectionStatus{}, fmt.Errorf("%w: %s", ErrTransactionProjectionNotFound, id)
	}
	if err != nil {
		return TransactionProjectionStatus{}, fmt.Errorf("get transaction projection status: %w", err)
	}
	status.EventID = eventID.String()
	return status, nil
}

func (s *PostgresStore) WaitForTransactionProjection(ctx context.Context, transactionID, consumer string, timeout time.Duration) (TransactionProjectionStatus, error) {
	return waitForCompletion(ctx, timeout, func() (TransactionProjectionStatus, bool, error) {
		status, err := s.GetTransactionProjectionStatus(ctx, transactionID, consumer)
		return status, status.Projected, err
	}, s.projectionWaiters, transactionID)
}

// ProjectionStatusStore composes the immutable OLTP outbox with a separately
// deployed projection database. The consumer inbox remains atomic with the
// aggregate rows on the projection side, while the outbox remains authoritative
// for whether a transaction emitted a projection event.
type ProjectionStatusStore struct {
	oltp       *PostgresStore
	projection *PostgresStore
}

func NewProjectionStatusStore(oltp, projection *PostgresStore) *ProjectionStatusStore {
	return &ProjectionStatusStore{oltp: oltp, projection: projection}
}

func (s *ProjectionStatusStore) GetTransactionProjectionStatus(ctx context.Context, transactionID, consumer string) (TransactionProjectionStatus, error) {
	if consumer == "" {
		return TransactionProjectionStatus{}, errors.New("consumer is required")
	}
	id, err := uuid.Parse(transactionID)
	if err != nil {
		return TransactionProjectionStatus{}, fmt.Errorf("invalid transaction ID: %w", err)
	}
	var eventID uuid.UUID
	if err := s.oltp.db.QueryRowContext(ctx, `
		SELECT id FROM outbox_events
		WHERE transaction_id=$1 AND event_type='transaction_posted'`, id).Scan(&eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TransactionProjectionStatus{}, fmt.Errorf("%w: %s", ErrTransactionProjectionNotFound, id)
		}
		return TransactionProjectionStatus{}, fmt.Errorf("get transaction projection event: %w", err)
	}
	status := TransactionProjectionStatus{TransactionID: id.String(), EventID: eventID.String(), Consumer: consumer}
	if err := s.projection.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM processed_events WHERE consumer=$1 AND event_id=$2)`, consumer, eventID).Scan(&status.Projected); err != nil {
		return TransactionProjectionStatus{}, fmt.Errorf("get projection inbox status: %w", err)
	}
	return status, nil
}

func (s *ProjectionStatusStore) WaitForTransactionProjection(ctx context.Context, transactionID, consumer string, timeout time.Duration) (TransactionProjectionStatus, error) {
	return waitForCompletion(ctx, timeout, func() (TransactionProjectionStatus, bool, error) {
		status, err := s.GetTransactionProjectionStatus(ctx, transactionID, consumer)
		return status, status.Projected, err
	}, s.oltp.projectionWaiters, transactionID)
}

func (s *PostgresStore) getDailyAggregates(ctx context.Context, query string, id uuid.UUID) (AggregateResponse, error) {
	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return AggregateResponse{}, err
	}
	defer rows.Close()
	response := AggregateResponse{Aggregates: []DailyAggregate{}}
	for rows.Next() {
		var aggregate DailyAggregate
		var debit, credit string
		var updated time.Time
		var eventID uuid.UUID
		if err := rows.Scan(&aggregate.Day, &debit, &credit, &updated, &eventID); err != nil {
			return AggregateResponse{}, err
		}
		aggregate.Debit, err = decimal.NewFromString(debit)
		if err != nil {
			return AggregateResponse{}, err
		}
		aggregate.Credit, err = decimal.NewFromString(credit)
		if err != nil {
			return AggregateResponse{}, err
		}
		response.Aggregates = append(response.Aggregates, aggregate)
		if updated.After(response.ProjectionTimestamp) {
			response.ProjectionTimestamp, response.LastEventID = updated, eventID.String()
		}
	}
	return response, rows.Err()
}

// RunAggregateConsumer uses a durable pull consumer; unacknowledged messages
// are redelivered after a process restart.
func RunAggregateConsumer(ctx context.Context, js nats.JetStreamContext, store *PostgresStore) error {
	return RunAggregateConsumerConcurrent(ctx, js, store, 1, 1)
}

// RunAggregateConsumerConcurrent shares one durable pull subscription across
// workers. Each event still commits and acknowledges independently, while
// unrelated ledgers can project in parallel.
func RunAggregateConsumerConcurrent(ctx context.Context, js nats.JetStreamContext, store *PostgresStore, workers, batch int) error {
	if workers <= 0 || batch <= 0 {
		return errors.New("aggregate workers and batch must be positive")
	}
	sub, err := js.PullSubscribe(TransactionPostedSubject, AggregateConsumerName, nats.BindStream(LedgerStreamName), nats.ManualAck())
	if err != nil {
		return err
	}
	errCh := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for ctx.Err() == nil {
				messageBatch, err := sub.FetchBatch(batch, nats.MaxWait(5*time.Millisecond))
				if err != nil {
					errCh <- err
					return
				}
				messages := make([]*nats.Msg, 0, batch)
				events := make([]TransactionPostedEvent, 0, batch)
				for msg := range messageBatch.Messages() {
					var event TransactionPostedEvent
					if err := json.Unmarshal(msg.Data, &event); err != nil {
						_ = msg.Term()
						continue
					}
					messages = append(messages, msg)
					events = append(events, event)
				}
				if len(events) > 0 {
					if _, err := store.ApplyTransactionPostedBatch(ctx, AggregateConsumerName, events); err != nil {
						for _, msg := range messages {
							_ = msg.Nak()
						}
						continue
					}
				}
				for _, msg := range messages {
					if err := msg.Ack(); err != nil {
						errCh <- err
						return
					}
				}
				if err := messageBatch.Error(); err != nil && !errors.Is(err, nats.ErrTimeout) {
					errCh <- err
					return
				}
			}
		}()
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		group.Wait()
		return ctx.Err()
	}
}
