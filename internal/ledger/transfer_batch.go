package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type destinationCreditBatchEvent struct {
	eventID            uuid.UUID
	sagaID             uuid.UUID
	destinationLedger  uuid.UUID
	destinationAccount uuid.UUID
	amount             decimal.Decimal
	clearingAccount    uuid.UUID
	transactionID      uuid.UUID
	destinationEntryID uuid.UUID
	clearingEntryID    uuid.UUID
}

// processDestinationCreditBatch completes independent transfer sagas in one
// PostgreSQL transaction. It creates every destination leg together, combines
// changes for shared accounts, and bulk-updates saga, step, and outbox state.
func (s *PostgresStore) processDestinationCreditBatch(ctx context.Context, limit int) (int, error) {
	tx, err := s.db.BeginTx(ctx, backgroundTxOptions)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT event.id,saga.id,saga.destination_ledger_id,saga.destination_account_id,saga.amount::text
		FROM outbox_events event
		JOIN sagas saga ON saga.id=event.saga_id
		WHERE event.status='pending' AND event.event_type='destination_credit'
		  AND event.available_at <= NOW() AND saga.status='pending'
		ORDER BY event.created_at,event.id
		FOR UPDATE OF event SKIP LOCKED
		LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	events := make([]destinationCreditBatchEvent, 0, limit)
	ledgerSet := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var event destinationCreditBatchEvent
		var amount string
		if err := rows.Scan(&event.eventID, &event.sagaID, &event.destinationLedger, &event.destinationAccount, &amount); err != nil {
			rows.Close()
			return 0, err
		}
		event.amount, err = decimal.NewFromString(amount)
		if err != nil {
			rows.Close()
			return 0, err
		}
		event.transactionID = uuid.New()
		event.destinationEntryID = uuid.New()
		event.clearingEntryID = uuid.New()
		events = append(events, event)
		ledgerSet[event.destinationLedger] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}

	ledgerIDs := sortedUUIDKeys(ledgerSet)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO accounts (ledger_id,name,type)
		SELECT ledger_id,'__transfer_clearing__','asset'
		FROM UNNEST($1::uuid[]) AS ledger_id
		ON CONFLICT (ledger_id,name) DO NOTHING`, ledgerIDs); err != nil {
		return 0, fmt.Errorf("ensure destination clearing accounts: %w", err)
	}
	clearingRows, err := tx.QueryContext(ctx, `SELECT ledger_id,id FROM accounts WHERE ledger_id=ANY($1::uuid[]) AND name='__transfer_clearing__'`, ledgerIDs)
	if err != nil {
		return 0, err
	}
	clearingByLedger := make(map[uuid.UUID]uuid.UUID, len(ledgerIDs))
	for clearingRows.Next() {
		var ledgerID, accountID uuid.UUID
		if err := clearingRows.Scan(&ledgerID, &accountID); err != nil {
			clearingRows.Close()
			return 0, err
		}
		clearingByLedger[ledgerID] = accountID
	}
	if err := clearingRows.Close(); err != nil {
		return 0, err
	}
	if err := clearingRows.Err(); err != nil {
		return 0, err
	}

	accountSet := make(map[uuid.UUID]struct{}, len(events)*2)
	for index := range events {
		clearing, ok := clearingByLedger[events[index].destinationLedger]
		if !ok {
			return 0, fmt.Errorf("destination clearing account missing for ledger %s", events[index].destinationLedger)
		}
		events[index].clearingAccount = clearing
		accountSet[events[index].destinationAccount] = struct{}{}
		accountSet[clearing] = struct{}{}
	}
	accountIDs := sortedUUIDKeys(accountSet)
	lockedRows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, accountIDs)
	if err != nil {
		return 0, err
	}
	locked := 0
	for lockedRows.Next() {
		locked++
	}
	if err := lockedRows.Close(); err != nil {
		return 0, err
	}
	if err := lockedRows.Err(); err != nil {
		return 0, err
	}
	if locked != len(accountIDs) {
		return 0, fmt.Errorf("destination batch locked %d accounts, want %d", locked, len(accountIDs))
	}

	if err := writeDestinationCreditBatch(ctx, tx, events); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, event := range events {
		s.transferWaiters.notify(event.sagaID.String())
	}
	return len(events), nil
}

func writeDestinationCreditBatch(ctx context.Context, tx *sql.Tx, events []destinationCreditBatchEvent) error {
	var query strings.Builder
	args := make([]any, 0, len(events)*12)
	query.WriteString(`WITH inserted_transactions AS (INSERT INTO transactions (id,ledger_id) VALUES `)
	for index, event := range events {
		if index > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::uuid)", base, base+1)
		args = append(args, event.transactionID, event.destinationLedger)
	}
	query.WriteString(` RETURNING id), inserted_entries AS (INSERT INTO entries (id,account_id,transaction_id,credit,debit) SELECT value.id,value.account_id,value.transaction_id,value.credit,value.debit FROM (VALUES `)
	entryIndex := 0
	for _, event := range events {
		for _, entry := range []struct {
			id      uuid.UUID
			account uuid.UUID
			credit  decimal.Decimal
			debit   decimal.Decimal
		}{
			{id: event.destinationEntryID, account: event.destinationAccount, debit: event.amount},
			{id: event.clearingEntryID, account: event.clearingAccount, credit: event.amount},
		} {
			if entryIndex > 0 {
				query.WriteByte(',')
			}
			entryIndex++
			base := len(args) + 1
			fmt.Fprintf(&query, "($%d::uuid,$%d::uuid,$%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2, base+3, base+4)
			args = append(args, entry.id, entry.account, event.transactionID, entry.credit.String(), entry.debit.String())
		}
	}
	query.WriteString(`) AS value(id,account_id,transaction_id,credit,debit) JOIN inserted_transactions ON inserted_transactions.id=value.transaction_id RETURNING id), updated_accounts AS (UPDATE accounts AS account SET balance=account.balance+change.delta FROM (VALUES `)
	deltas := make(map[uuid.UUID]decimal.Decimal)
	for _, event := range events {
		deltas[event.destinationAccount] = deltas[event.destinationAccount].Add(event.amount)
		deltas[event.clearingAccount] = deltas[event.clearingAccount].Sub(event.amount)
	}
	accountIDs := make([]uuid.UUID, 0, len(deltas))
	for accountID := range deltas {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(i, j int) bool { return bytes.Compare(accountIDs[i][:], accountIDs[j][:]) < 0 })
	for index, accountID := range accountIDs {
		if index > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::numeric)", base, base+1)
		args = append(args, accountID, deltas[accountID].String())
	}
	query.WriteString(`) AS change(id,delta) WHERE account.id=change.id RETURNING account.id), inserted_outbox AS (INSERT INTO outbox_events (transaction_id,event_type,status,available_at) SELECT id,'transaction_posted','pending',NOW() FROM inserted_transactions RETURNING id), completed_sagas AS (UPDATE sagas SET status='completed',updated_at=NOW() WHERE id=ANY($`)
	sagaIDs := make([]uuid.UUID, len(events))
	eventIDs := make([]uuid.UUID, len(events))
	for index, event := range events {
		sagaIDs[index] = event.sagaID
		eventIDs[index] = event.eventID
	}
	sagaArg := len(args) + 1
	args = append(args, sagaIDs)
	fmt.Fprintf(&query, "%d::uuid[]) RETURNING id), completed_steps AS (UPDATE saga_steps SET status='completed',attempt_count=attempt_count+1,error=NULL,updated_at=NOW() WHERE saga_id=ANY($%d::uuid[]) AND step='destination_credit' RETURNING saga_id) UPDATE outbox_events SET status='processed',processed_at=NOW(),attempt_count=attempt_count+1 WHERE id=ANY($", sagaArg, sagaArg)
	eventArg := len(args) + 1
	args = append(args, eventIDs)
	fmt.Fprintf(&query, "%d::uuid[])", eventArg)

	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return fmt.Errorf("write destination credit batch: %w", err)
	}
	return nil
}

func sortedUUIDKeys(values map[uuid.UUID]struct{}) []uuid.UUID {
	keys := make([]uuid.UUID, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	return keys
}
