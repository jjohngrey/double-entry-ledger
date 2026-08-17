package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
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
	afterBalanceCheck  func(uuid.UUID)
	retryAttempts      atomic.Uint64
	retryExhausted     atomic.Uint64
	sleep              func(time.Duration)
	transactionAttempt func(string, []EntryRequest) (*Transaction, bool, error)
}

func NewPostgresStore(sqlDB *sql.DB) *PostgresStore {
	return &PostgresStore{
		db:        sqlDB,
		txOptions: &sql.TxOptions{Isolation: sql.LevelSerializable},
		sleep:     time.Sleep,
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
		ID:      id.String(),
		Name:    name,
		Type:    accType,
		Balance: decimal.Zero,
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
	if err := s.ensureSufficientBalances(ctx, tx, preparedEntries); err != nil {
		return nil, false, err
	}

	txID, err := q.CreateTransaction(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("create transaction row: %w", err)
	}

	var ledgerEntries []Entry
	for _, e := range preparedEntries {
		entryID, err := q.CreateEntry(ctx, db.CreateEntryParams{
			AccountID:     e.accountID,
			TransactionID: txID,
			Credit:        e.entry.Credit.String(),
			Debit:         e.entry.Debit.String(),
		})
		if err != nil {
			return nil, false, fmt.Errorf("create entry: %w", err)
		}

		_, err = q.UpdateAccountBalance(ctx, db.UpdateAccountBalanceParams{
			ID:        e.accountID,
			Balance:   e.entry.Debit.String(),
			Balance_2: e.entry.Credit.String(),
		})
		if err != nil {
			return nil, false, fmt.Errorf("update account balance: %w", err)
		}

		ledgerEntries = append(ledgerEntries, Entry{
			ID:            entryID.String(),
			AccountID:     e.entry.AccountID,
			TransactionID: txID.String(),
			Credit:        e.entry.Credit,
			Debit:         e.entry.Debit,
		})
	}

	if idempotencyKey != "" {
		if err := q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{
			Key:           idempotencyKey,
			TransactionID: uuid.NullUUID{UUID: txID, Valid: true},
		}); err != nil {
			return nil, false, fmt.Errorf("complete idempotency key: %w", err)
		}
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
	return &PostgresStore{db: sqlDB, txOptions: txOptions, sleep: time.Sleep}
}

type postgresEntry struct {
	accountID uuid.UUID
	entry     EntryRequest
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

func (s *PostgresStore) ensureSufficientBalances(ctx context.Context, tx *sql.Tx, entries []postgresEntry) error {
	changes := make(map[uuid.UUID]EntryRequest)
	for _, e := range entries {
		change := changes[e.accountID]
		change.AccountID = e.entry.AccountID
		change.Credit = change.Credit.Add(e.entry.Credit)
		change.Debit = change.Debit.Add(e.entry.Debit)
		changes[e.accountID] = change
	}

	for accountID, change := range changes {
		if err := ensureSufficientBalance(ctx, tx, accountID, change); err != nil {
			return err
		}
		if s.afterBalanceCheck != nil {
			s.afterBalanceCheck(accountID)
		}
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
