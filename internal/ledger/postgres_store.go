package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jjohngrey/double-entry-ledger/internal/db"
	"github.com/shopspring/decimal"
)

const (
	maxSerializationRetries = 3
	retryBaseDelay          = 10 * time.Millisecond
)

// ErrTransactionRetryExhausted is returned after PostgreSQL serialization
// failures have used all retry attempts. Its text is safe to return to clients.
var ErrTransactionRetryExhausted = errors.New("transaction temporarily unavailable; please retry")

type TransactionRetryMetrics struct {
	Attempts  uint64
	Exhausted uint64
}

type PostgresStore struct {
	db                 *sql.DB
	txOptions          *sql.TxOptions
	beforeAccountLock  func(uuid.UUID)
	retryAttempts      atomic.Uint64
	retryExhausted     atomic.Uint64
	sleep              func(time.Duration)
	transactionAttempt func(string, []EntryRequest) (*Transaction, bool, error)
	projectionWaiters  *completionWaiters
	transferWaiters    *completionWaiters
}

func NewPostgresStore(sqlDB *sql.DB) *PostgresStore {
	return &PostgresStore{
		db: sqlDB,
		// Posting locks every affected account in deterministic UUID order.
		// READ COMMITTED therefore preserves overdraft safety without SSI's
		// page-level false conflicts on unrelated idempotency/outbox inserts.
		txOptions:         &sql.TxOptions{Isolation: sql.LevelReadCommitted},
		sleep:             time.Sleep,
		projectionWaiters: newCompletionWaiters(),
		transferWaiters:   newCompletionWaiters(),
	}
}

// WithDatabase creates a store for a dedicated connection pool while sharing
// the in-process completion notifications used by foreground long polls. The
// database remains authoritative; sharing only avoids an unnecessary final
// status read after a background worker commits.
func (s *PostgresStore) WithDatabase(sqlDB *sql.DB) *PostgresStore {
	return &PostgresStore{
		db:                sqlDB,
		txOptions:         s.txOptions,
		sleep:             s.sleep,
		projectionWaiters: s.projectionWaiters,
		transferWaiters:   s.transferWaiters,
	}
}

var _ Store = (*PostgresStore)(nil)

func (s *PostgresStore) CreateAccount(name string, accType AccountType) (*Account, error) {
	if err := ValidateAccount(name, accType); err != nil {
		return nil, err
	}

	ctx := context.Background()
	q := db.New(s.db)

	id, err := q.CreateAccount(ctx, db.CreateAccountParams{
		Name: name,
		Type: db.AccountType(accType),
	})
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}

	return &Account{
		ID: id.String(), LedgerID: DefaultLedgerID, Name: name, Type: accType, Balance: decimal.Zero,
	}, nil
}

func (s *PostgresStore) GetBalance(accountID string) (decimal.Decimal, error) {
	id, err := uuid.Parse(accountID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid account ID: %w", err)
	}

	ctx := context.Background()
	q := db.New(s.db)

	balStr, err := q.GetAccountBalance(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return decimal.Zero, fmt.Errorf("Account with ID %s not found", accountID)
		}
		return decimal.Zero, fmt.Errorf("get balance: %w", err)
	}

	return decimal.NewFromString(balStr)
}

func (s *PostgresStore) CreateTransaction(idempotencyKey string, entries []EntryRequest) (*Transaction, bool, error) {
	if err := ValidateTransaction(entries); err != nil {
		return nil, false, err
	}
	checksum := TransactionRequestChecksum("", entries)
	if idempotencyKey != "" {
		existing, found, err := s.completedIdempotentTransaction(context.Background(), idempotencyKey, checksum)
		if err != nil || found {
			return existing, found, err
		}
	}

	for retry := 0; ; retry++ {
		var transaction *Transaction
		var existed bool
		var err error
		if s.transactionAttempt != nil {
			transaction, existed, err = s.transactionAttempt(idempotencyKey, entries)
		} else {
			transaction, existed, err = s.createTransactionAttempt(idempotencyKey, checksum, entries)
		}
		if err == nil {
			return transaction, existed, nil
		}
		if !isSerializationFailure(err) {
			return nil, false, err
		}
		if retry == maxSerializationRetries {
			s.retryExhausted.Add(1)
			log.Printf("ledger transaction serialization retry exhausted retries=%d", retry)
			return nil, false, ErrTransactionRetryExhausted
		}

		s.retryAttempts.Add(1)
		log.Printf("ledger transaction serialization retry attempt=%d", retry+1)
		s.sleep(serializationRetryDelay(retry))
	}
}

// completedIdempotentTransaction keeps the overwhelmingly common duplicate
// path read-only. The claim inside createTransactionAttempt remains the
// authority for missing or in-flight keys, so concurrent first requests are
// still atomic.
func (s *PostgresStore) completedIdempotentTransaction(ctx context.Context, key, checksum string) (*Transaction, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT k.transaction_id,k.request_checksum,k.status,t.created_at,
		       e.id,e.account_id,e.credit::text,e.debit::text
		FROM idempotency_keys k
		LEFT JOIN transactions t ON t.id=k.transaction_id
		LEFT JOIN entries e ON e.transaction_id=k.transaction_id
		WHERE k.key=$1
		ORDER BY e.id`, key)
	if err != nil {
		return nil, false, fmt.Errorf("read idempotency key: %w", err)
	}
	defer rows.Close()
	var transactionID uuid.NullUUID
	var storedChecksum, status string
	var timestamp sql.NullTime
	transaction := &Transaction{}
	found := false
	for rows.Next() {
		found = true
		var entryID, accountID uuid.NullUUID
		var credit, debit sql.NullString
		if err := rows.Scan(&transactionID, &storedChecksum, &status, &timestamp, &entryID, &accountID, &credit, &debit); err != nil {
			return nil, false, fmt.Errorf("scan idempotency key: %w", err)
		}
		if storedChecksum != checksum {
			return nil, false, ErrIdempotencyKeyConflict
		}
		if entryID.Valid {
			parsedCredit, err := decimal.NewFromString(credit.String)
			if err != nil {
				return nil, false, fmt.Errorf("parse credit: %w", err)
			}
			parsedDebit, err := decimal.NewFromString(debit.String)
			if err != nil {
				return nil, false, fmt.Errorf("parse debit: %w", err)
			}
			transaction.Entries = append(transaction.Entries, Entry{ID: entryID.UUID.String(), AccountID: accountID.UUID.String(), TransactionID: transactionID.UUID.String(), Credit: parsedCredit, Debit: parsedDebit})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read idempotency entries: %w", err)
	}
	if !found || status != "completed" || !transactionID.Valid {
		return nil, false, nil
	}
	transaction.ID = transactionID.UUID.String()
	if timestamp.Valid {
		transaction.Timestamp = timestamp.Time
	}
	return transaction, true, nil
}

func (s *PostgresStore) createTransactionAttempt(idempotencyKey, checksum string, entries []EntryRequest) (*Transaction, bool, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, s.txOptions)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	q := db.New(tx)

	if idempotencyKey != "" {
		claim, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{
			Key:             idempotencyKey,
			RequestChecksum: checksum,
		})
		if err != nil {
			return nil, false, fmt.Errorf("claim idempotency key: %w", err)
		}
		if claim.RequestChecksum != checksum {
			return nil, false, ErrIdempotencyKeyConflict
		}
		if claim.TransactionID.Valid {
			existing, err := s.buildTransaction(q, ctx, claim.TransactionID.UUID)
			if err != nil {
				return nil, false, err
			}
			return existing, true, nil
		}
	}

	preparedEntries, err := preparePostgresEntries(entries)
	if err != nil {
		return nil, false, err
	}
	ledgerID, accountChanges, err := s.lockPostingAccounts(ctx, tx, preparedEntries)
	if err != nil {
		return nil, false, err
	}

	txID := uuid.New()
	ledgerEntries, err := writePosting(ctx, tx, txID, ledgerID, idempotencyKey, preparedEntries, accountChanges)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}

	return &Transaction{
		ID:        txID.String(),
		Entries:   ledgerEntries,
		Timestamp: time.Now(),
	}, false, nil
}

// writePosting collapses the transaction, entry, balance, idempotency, and
// outbox writes into one PostgreSQL command. Account validation and locking
// still happen first, and any CTE failure rolls the entire statement back.
func writePosting(ctx context.Context, tx *sql.Tx, transactionID, ledgerID uuid.UUID, idempotencyKey string, entries []postgresEntry, changes []postingAccountChange) ([]Entry, error) {
	var query strings.Builder
	query.WriteString(`WITH inserted_transaction AS (INSERT INTO transactions (id,ledger_id) VALUES ($1,$2) RETURNING id), inserted_entries AS (INSERT INTO entries (id,account_id,transaction_id,credit,debit) SELECT value.id,value.account_id,inserted_transaction.id,value.credit,value.debit FROM (VALUES `)
	args := []any{transactionID, ledgerID}
	ledgerEntries := make([]Entry, len(entries))
	for i, entry := range entries {
		if i > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2, base+3)
		entryID := uuid.New()
		args = append(args, entryID, entry.accountID, entry.entry.Credit.String(), entry.entry.Debit.String())
		ledgerEntries[i] = Entry{ID: entryID.String(), AccountID: entry.accountID.String(), TransactionID: transactionID.String(), Credit: entry.entry.Credit, Debit: entry.entry.Debit}
	}
	query.WriteString(`) AS value(id,account_id,credit,debit) CROSS JOIN inserted_transaction RETURNING id), updated_accounts AS (UPDATE accounts AS account SET balance=account.balance+change.debit-change.credit FROM (VALUES `)
	for i, change := range changes {
		if i > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2)
		args = append(args, change.accountID, change.debit.String(), change.credit.String())
	}
	query.WriteString(`) AS change(id,debit,credit) WHERE account.id=change.id RETURNING account.id)`)
	if idempotencyKey != "" {
		keyParameter := len(args) + 1
		fmt.Fprintf(&query, `, completed_key AS (UPDATE idempotency_keys SET transaction_id=inserted_transaction.id,status='completed',updated_at=NOW() FROM inserted_transaction WHERE key=$%d RETURNING key)`, keyParameter)
		args = append(args, idempotencyKey)
	}
	query.WriteString(` INSERT INTO outbox_events (transaction_id,event_type,status,available_at) SELECT id,'transaction_posted','pending',NOW() FROM inserted_transaction`)
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return nil, fmt.Errorf("write posting: %w", err)
	}
	return ledgerEntries, nil
}

func (s *PostgresStore) RetryMetrics() TransactionRetryMetrics {
	return TransactionRetryMetrics{
		Attempts:  s.retryAttempts.Load(),
		Exhausted: s.retryExhausted.Load(),
	}
}

func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

func serializationRetryDelay(retry int) time.Duration {
	backoff := retryBaseDelay * time.Duration(1<<retry)
	return backoff/2 + time.Duration(rand.Int64N(int64(backoff/2)+1))
}

func (s *PostgresStore) GetAccountEntries(accountID string, params GetAccountEntriesParams) (GetAccountEntriesResponse, error) {
	id, err := uuid.Parse(accountID)
	if err != nil {
		return GetAccountEntriesResponse{}, fmt.Errorf("invalid account ID: %w", err)
	}

	ctx := context.Background()
	q := db.New(s.db)

	dbEntries, err := q.GetAccountEntries(ctx, db.GetAccountEntriesParams{
		AccountID:   id,
		CreatedAt:   params.From,
		CreatedAt_2: params.To,
	})
	if err != nil {
		return GetAccountEntriesResponse{}, fmt.Errorf("get account entries: %w", err)
	}

	entries := make([]Entry, len(dbEntries))
	runningBalance := decimal.Zero
	for i, e := range dbEntries {
		credit, err := decimal.NewFromString(e.Credit)
		if err != nil {
			return GetAccountEntriesResponse{}, fmt.Errorf("parse credit: %w", err)
		}
		debit, err := decimal.NewFromString(e.Debit)
		if err != nil {
			return GetAccountEntriesResponse{}, fmt.Errorf("parse debit: %w", err)
		}
		entries[i] = Entry{
			ID:            e.ID.String(),
			AccountID:     e.AccountID.String(),
			TransactionID: e.TransactionID.String(),
			Credit:        credit,
			Debit:         debit,
		}
		runningBalance = runningBalance.Add(debit).Sub(credit)
	}

	return GetAccountEntriesResponse{
		Entries:        entries,
		RunningBalance: runningBalance,
	}, nil
}

func newPostgresStoreWithTxOptions(sqlDB *sql.DB, txOptions *sql.TxOptions) *PostgresStore {
	return &PostgresStore{db: sqlDB, txOptions: txOptions, sleep: time.Sleep, projectionWaiters: newCompletionWaiters(), transferWaiters: newCompletionWaiters()}
}

type postgresEntry struct {
	accountID uuid.UUID
	entry     EntryRequest
}

type postingAccountChange struct {
	accountID uuid.UUID
	debit     decimal.Decimal
	credit    decimal.Decimal
}

func preparePostgresEntries(entries []EntryRequest) ([]postgresEntry, error) {
	prepared := make([]postgresEntry, len(entries))
	for i, entry := range entries {
		accountID, err := uuid.Parse(entry.AccountID)
		if err != nil {
			return nil, fmt.Errorf("invalid account ID %s: %w", entry.AccountID, err)
		}
		prepared[i] = postgresEntry{
			accountID: accountID,
			entry:     entry,
		}
	}
	return prepared, nil
}

// lockPostingAccounts replaces the per-entry ledger and balance lookups with
// one deterministic row-locking query. Aggregating repeated account IDs before
// validation preserves the same final-balance rule as applying every entry.
func (s *PostgresStore) lockPostingAccounts(ctx context.Context, tx *sql.Tx, entries []postgresEntry) (uuid.UUID, []postingAccountChange, error) {
	changesByID := make(map[uuid.UUID]postingAccountChange, len(entries))
	for _, entry := range entries {
		change := changesByID[entry.accountID]
		change.accountID = entry.accountID
		change.debit = change.debit.Add(entry.entry.Debit)
		change.credit = change.credit.Add(entry.entry.Credit)
		changesByID[entry.accountID] = change
	}

	changes := make([]postingAccountChange, 0, len(changesByID))
	for _, change := range changesByID {
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool {
		return bytes.Compare(changes[i].accountID[:], changes[j].accountID[:]) < 0
	})
	for _, change := range changes {
		if s.beforeAccountLock != nil {
			s.beforeAccountLock(change.accountID)
		}
	}

	var query strings.Builder
	query.WriteString(`SELECT id, ledger_id, type::text, balance::text FROM accounts WHERE id IN (`)
	args := make([]any, 0, len(changes))
	for i, change := range changes {
		if i > 0 {
			query.WriteByte(',')
		}
		fmt.Fprintf(&query, "$%d", i+1)
		args = append(args, change.accountID)
	}
	query.WriteString(`) ORDER BY id FOR UPDATE`)

	rows, err := tx.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("lock posting accounts: %w", err)
	}
	defer rows.Close()

	var ledgerID uuid.UUID
	found := 0
	for rows.Next() {
		var accountID, accountLedgerID uuid.UUID
		var accountTypeString, balanceString string
		if err := rows.Scan(&accountID, &accountLedgerID, &accountTypeString, &balanceString); err != nil {
			return uuid.Nil, nil, fmt.Errorf("scan posting account: %w", err)
		}
		change, ok := changesByID[accountID]
		if !ok {
			return uuid.Nil, nil, fmt.Errorf("unexpected posting account: %s", accountID)
		}
		storedBalance, err := decimal.NewFromString(balanceString)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("parse account balance: %w", err)
		}
		if displayedBalanceAfter(AccountType(accountTypeString), storedBalance, EntryRequest{
			AccountID: accountID.String(), Debit: change.debit, Credit: change.credit,
		}).IsNegative() {
			return uuid.Nil, nil, fmt.Errorf("insufficient funds for account %s", accountID)
		}
		if found == 0 {
			ledgerID = accountLedgerID
		} else if accountLedgerID != ledgerID {
			return uuid.Nil, nil, errors.New("a transaction cannot span ledger boundaries")
		}
		found++
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, nil, fmt.Errorf("read posting accounts: %w", err)
	}
	if found != len(changes) {
		for _, change := range changes {
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id=$1)`, change.accountID).Scan(&exists); err != nil {
				return uuid.Nil, nil, fmt.Errorf("verify posting account: %w", err)
			}
			if !exists {
				return uuid.Nil, nil, fmt.Errorf("account not found: %s", change.accountID)
			}
		}
		return uuid.Nil, nil, errors.New("one or more posting accounts were not found")
	}
	return ledgerID, changes, nil
}

func insertPostingEntries(ctx context.Context, tx *sql.Tx, transactionID uuid.UUID, entries []postgresEntry) ([]Entry, error) {
	var query strings.Builder
	query.WriteString(`INSERT INTO entries (id,account_id,transaction_id,credit,debit) VALUES `)
	args := make([]any, 0, len(entries)*5)
	result := make([]Entry, len(entries))
	for i, entry := range entries {
		if i > 0 {
			query.WriteByte(',')
		}
		base := i*5 + 1
		fmt.Fprintf(&query, "($%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4)
		entryID := uuid.New()
		args = append(args, entryID, entry.accountID, transactionID, entry.entry.Credit.String(), entry.entry.Debit.String())
		result[i] = Entry{
			ID: entryID.String(), AccountID: entry.entry.AccountID, TransactionID: transactionID.String(),
			Credit: entry.entry.Credit, Debit: entry.entry.Debit,
		}
	}
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return nil, fmt.Errorf("create entries: %w", err)
	}
	return result, nil
}

func updatePostingBalances(ctx context.Context, tx *sql.Tx, changes []postingAccountChange) error {
	var query strings.Builder
	query.WriteString(`UPDATE accounts AS account SET balance=account.balance+change.debit-change.credit FROM (VALUES `)
	args := make([]any, 0, len(changes)*3)
	for i, change := range changes {
		if i > 0 {
			query.WriteByte(',')
		}
		base := i*3 + 1
		fmt.Fprintf(&query, "($%d::uuid,$%d::numeric,$%d::numeric)", base, base+1, base+2)
		args = append(args, change.accountID, change.debit.String(), change.credit.String())
	}
	query.WriteString(`) AS change(id,debit,credit) WHERE account.id=change.id`)
	result, err := tx.ExecContext(ctx, query.String(), args...)
	if err != nil {
		return fmt.Errorf("update account balances: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated account balances: %w", err)
	}
	if updated != int64(len(changes)) {
		return fmt.Errorf("updated %d account balances, expected %d", updated, len(changes))
	}
	return nil
}

func ensureSufficientBalance(ctx context.Context, tx *sql.Tx, accountID uuid.UUID, entry EntryRequest) error {
	var accountTypeStr string
	var balanceStr string
	err := tx.QueryRowContext(ctx, `
		SELECT type, CAST(balance AS NUMERIC(20,2))
		FROM accounts
		WHERE id = $1
		FOR UPDATE
	`, accountID).Scan(&accountTypeStr, &balanceStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("account not found: %s", accountID)
		}
		return fmt.Errorf("get account balance for posting: %w", err)
	}

	storedBalance, err := decimal.NewFromString(balanceStr)
	if err != nil {
		return fmt.Errorf("parse account balance: %w", err)
	}

	accountType := AccountType(accountTypeStr)
	newDisplayBalance := displayedBalanceAfter(accountType, storedBalance, entry)
	if newDisplayBalance.IsNegative() {
		return fmt.Errorf("insufficient funds for account %s", accountID)
	}

	return nil
}

func displayedBalanceAfter(accountType AccountType, storedBalance decimal.Decimal, entry EntryRequest) decimal.Decimal {
	switch accountType {
	case AssetType, ExpenseType:
		return storedBalance.Add(entry.Debit).Sub(entry.Credit)
	default:
		return storedBalance.Neg().Add(entry.Credit).Sub(entry.Debit)
	}
}

func (s *PostgresStore) buildTransaction(q *db.Queries, ctx context.Context, txID uuid.UUID) (*Transaction, error) {
	dbEntries, err := q.GetEntriesByTransactionID(ctx, txID)
	if err != nil {
		return nil, fmt.Errorf("fetch entries: %w", err)
	}

	entries := make([]Entry, len(dbEntries))
	for i, e := range dbEntries {
		credit, err := decimal.NewFromString(e.Credit)
		if err != nil {
			return nil, fmt.Errorf("parse credit: %w", err)
		}
		debit, err := decimal.NewFromString(e.Debit)
		if err != nil {
			return nil, fmt.Errorf("parse debit: %w", err)
		}
		entries[i] = Entry{
			ID:            e.ID.String(),
			AccountID:     e.AccountID.String(),
			TransactionID: txID.String(),
			Credit:        credit,
			Debit:         debit,
		}
	}

	return &Transaction{
		ID:        txID.String(),
		Entries:   entries,
		Timestamp: time.Now(),
	}, nil
}
