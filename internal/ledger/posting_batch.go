package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const maxPostingBatchSize = 64

type BatchPostingResult struct {
	Transaction *Transaction
	Err         error
}

type preparedBatchPosting struct {
	index    int
	ledgerID uuid.UUID
	entries  []postgresEntry
}

type batchAccountState struct {
	ledgerID uuid.UUID
	typ      AccountType
	balance  decimal.Decimal
}

// CreateTransactionsBatch posts independent, non-idempotent transactions in
// one database transaction. Invalid operations are excluded without poisoning
// valid neighbors; accepted operations share one commit and WAL flush.
func (s *PostgresStore) CreateTransactionsBatch(entrySets [][]EntryRequest) ([]BatchPostingResult, error) {
	if len(entrySets) == 0 || len(entrySets) > maxPostingBatchSize {
		return nil, fmt.Errorf("posting batch size must be from 1 through %d", maxPostingBatchSize)
	}
	results := make([]BatchPostingResult, len(entrySets))
	prepared := make([]preparedBatchPosting, 0, len(entrySets))
	accountSet := make(map[uuid.UUID]struct{})
	for index, entries := range entrySets {
		if err := ValidateTransaction(entries); err != nil {
			results[index].Err = err
			continue
		}
		postgresEntries, err := preparePostgresEntries(entries)
		if err != nil {
			results[index].Err = err
			continue
		}
		for _, entry := range postgresEntries {
			accountSet[entry.accountID] = struct{}{}
		}
		prepared = append(prepared, preparedBatchPosting{index: index, entries: postgresEntries})
	}
	if len(prepared) == 0 {
		return results, nil
	}

	accountIDs := make([]uuid.UUID, 0, len(accountSet))
	for accountID := range accountSet {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Slice(accountIDs, func(i, j int) bool { return bytes.Compare(accountIDs[i][:], accountIDs[j][:]) < 0 })
	for _, accountID := range accountIDs {
		if s.beforeAccountLock != nil {
			s.beforeAccountLock(accountID)
		}
	}

	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, s.txOptions)
	if err != nil {
		return nil, fmt.Errorf("begin posting batch: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,ledger_id,type::text,balance::text FROM accounts WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("lock posting batch accounts: %w", err)
	}
	states := make(map[uuid.UUID]batchAccountState, len(accountIDs))
	for rows.Next() {
		var accountID, ledgerID uuid.UUID
		var accountType, balanceText string
		if err := rows.Scan(&accountID, &ledgerID, &accountType, &balanceText); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan posting batch account: %w", err)
		}
		balance, err := decimal.NewFromString(balanceText)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("parse posting batch balance: %w", err)
		}
		states[accountID] = batchAccountState{ledgerID: ledgerID, typ: AccountType(accountType), balance: balance}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	accepted := make([]preparedBatchPosting, 0, len(prepared))
	totalChanges := make(map[uuid.UUID]postingAccountChange)
	for _, posting := range prepared {
		changes := make(map[uuid.UUID]postingAccountChange)
		var ledgerID uuid.UUID
		valid := true
		for _, entry := range posting.entries {
			state, exists := states[entry.accountID]
			if !exists {
				results[posting.index].Err = fmt.Errorf("account not found: %s", entry.accountID)
				valid = false
				break
			}
			if ledgerID == uuid.Nil {
				ledgerID = state.ledgerID
			} else if state.ledgerID != ledgerID {
				results[posting.index].Err = errors.New("a transaction cannot span ledger boundaries")
				valid = false
				break
			}
			change := changes[entry.accountID]
			change.accountID = entry.accountID
			change.debit = change.debit.Add(entry.entry.Debit)
			change.credit = change.credit.Add(entry.entry.Credit)
			changes[entry.accountID] = change
		}
		if !valid {
			continue
		}
		for accountID, change := range changes {
			state := states[accountID]
			newBalance := state.balance.Add(change.debit).Sub(change.credit)
			displayBalance := newBalance
			if state.typ != AssetType && state.typ != ExpenseType {
				displayBalance = newBalance.Neg()
			}
			if displayBalance.IsNegative() {
				results[posting.index].Err = fmt.Errorf("insufficient funds for account %s", accountID)
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		for accountID, change := range changes {
			state := states[accountID]
			state.balance = state.balance.Add(change.debit).Sub(change.credit)
			states[accountID] = state
			total := totalChanges[accountID]
			total.accountID = accountID
			total.debit = total.debit.Add(change.debit)
			total.credit = total.credit.Add(change.credit)
			totalChanges[accountID] = total
		}
		posting.ledgerID = ledgerID
		accepted = append(accepted, posting)
	}

	if len(accepted) > 0 {
		if err := writePostingBatch(ctx, tx, accepted, totalChanges, results); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit posting batch: %w", err)
	}
	committedAt := time.Now()
	for index := range results {
		if results[index].Transaction != nil {
			results[index].Transaction.Timestamp = committedAt
		}
	}
	return results, nil
}

func writePostingBatch(ctx context.Context, tx *sql.Tx, postings []preparedBatchPosting, changes map[uuid.UUID]postingAccountChange, results []BatchPostingResult) error {
	var query strings.Builder
	query.WriteString(`WITH inserted_transactions AS (INSERT INTO transactions (id,ledger_id) VALUES `)
	args := make([]any, 0)
	type transactionWrite struct {
		id      uuid.UUID
		posting preparedBatchPosting
	}
	writes := make([]transactionWrite, len(postings))
	for index, posting := range postings {
		if index > 0 {
			query.WriteByte(',')
		}
		transactionID := uuid.New()
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::uuid)", base, base+1)
		args = append(args, transactionID, posting.ledgerID)
		writes[index] = transactionWrite{id: transactionID, posting: posting}
		results[posting.index].Transaction = &Transaction{ID: transactionID.String(), LedgerID: posting.ledgerID.String(), Entries: make([]Entry, len(posting.entries))}
	}
	query.WriteString(` RETURNING id), inserted_entries AS (INSERT INTO entries (id,account_id,transaction_id,credit,debit) SELECT value.id,value.account_id,value.transaction_id,value.credit,value.debit FROM (VALUES `)
	entryIndex := 0
	for _, write := range writes {
		for index, entry := range write.posting.entries {
			if entryIndex > 0 {
				query.WriteByte(',')
			}
			entryIndex++
			entryID := uuid.New()
			base := len(args) + 1
			fmt.Fprintf(&query, "($%d::uuid,$%d::uuid,$%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2, base+3, base+4)
			args = append(args, entryID, entry.accountID, write.id, entry.entry.Credit.String(), entry.entry.Debit.String())
			results[write.posting.index].Transaction.Entries[index] = Entry{ID: entryID.String(), AccountID: entry.accountID.String(), TransactionID: write.id.String(), Credit: entry.entry.Credit, Debit: entry.entry.Debit}
		}
	}
	query.WriteString(`) AS value(id,account_id,transaction_id,credit,debit) JOIN inserted_transactions ON inserted_transactions.id=value.transaction_id RETURNING id), updated_accounts AS (UPDATE accounts AS account SET balance=account.balance+change.debit-change.credit FROM (VALUES `)
	changeList := make([]postingAccountChange, 0, len(changes))
	for _, change := range changes {
		changeList = append(changeList, change)
	}
	sort.Slice(changeList, func(i, j int) bool { return bytes.Compare(changeList[i].accountID[:], changeList[j].accountID[:]) < 0 })
	for index, change := range changeList {
		if index > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2)
		args = append(args, change.accountID, change.debit.String(), change.credit.String())
	}
	query.WriteString(`) AS change(id,debit,credit) WHERE account.id=change.id RETURNING account.id) INSERT INTO outbox_events (transaction_id,event_type,status,available_at) SELECT id,'transaction_posted','pending',NOW() FROM inserted_transactions`)
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return fmt.Errorf("write posting batch: %w", err)
	}
	return nil
}

type postingBatchRequest struct {
	entries  []EntryRequest
	response chan postingBatchResponse
}

type postingBatchResponse struct {
	transaction *Transaction
	err         error
}

// BatchingPostgresStore preserves Store's existing API while micro-batching
// only non-idempotent posts. Idempotent requests retain their dedicated claim
// path and response semantics.
type BatchingPostgresStore struct {
	store     *PostgresStore
	requests  chan postingBatchRequest
	maxBatch  int
	maxWait   time.Duration
	closeOnce sync.Once
	workers   sync.WaitGroup
}

func NewBatchingPostgresStore(store *PostgresStore, maxBatch int, maxWait time.Duration, workers int) (*BatchingPostgresStore, error) {
	if store == nil || maxBatch <= 0 || maxBatch > maxPostingBatchSize || maxWait < 0 || workers <= 0 {
		return nil, errors.New("invalid posting batch configuration")
	}
	batching := &BatchingPostgresStore{store: store, requests: make(chan postingBatchRequest, maxBatch*workers), maxBatch: maxBatch, maxWait: maxWait}
	batching.workers.Add(workers)
	for range workers {
		go batching.run()
	}
	return batching, nil
}

func (s *BatchingPostgresStore) Close() {
	s.closeOnce.Do(func() { close(s.requests) })
	s.workers.Wait()
}

func (s *BatchingPostgresStore) CreateTransaction(idempotencyKey string, entries []EntryRequest) (*Transaction, bool, error) {
	if idempotencyKey != "" {
		return s.store.CreateTransaction(idempotencyKey, entries)
	}
	if err := ValidateTransaction(entries); err != nil {
		return nil, false, err
	}
	response := make(chan postingBatchResponse, 1)
	s.requests <- postingBatchRequest{entries: entries, response: response}
	result := <-response
	return result.transaction, false, result.err
}

func (s *BatchingPostgresStore) run() {
	defer s.workers.Done()
	for {
		first, ok := <-s.requests
		if !ok {
			return
		}
		batch := []postingBatchRequest{first}
		timer := time.NewTimer(s.maxWait)
	collect:
		for len(batch) < s.maxBatch {
			select {
			case request, ok := <-s.requests:
				if !ok {
					break collect
				}
				batch = append(batch, request)
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		entrySets := make([][]EntryRequest, len(batch))
		for index, request := range batch {
			entrySets[index] = request.entries
		}
		results, err := s.store.CreateTransactionsBatch(entrySets)
		for index, request := range batch {
			response := postingBatchResponse{err: err}
			if err == nil {
				response.transaction, response.err = results[index].Transaction, results[index].Err
			}
			request.response <- response
		}
	}
}

func (s *BatchingPostgresStore) CreateAccount(name string, typ AccountType) (*Account, error) {
	return s.store.CreateAccount(name, typ)
}

func (s *BatchingPostgresStore) GetBalance(accountID string) (decimal.Decimal, error) {
	return s.store.GetBalance(accountID)
}

func (s *BatchingPostgresStore) GetAccountEntries(accountID string, params GetAccountEntriesParams) (GetAccountEntriesResponse, error) {
	return s.store.GetAccountEntries(accountID, params)
}

var _ Store = (*BatchingPostgresStore)(nil)
