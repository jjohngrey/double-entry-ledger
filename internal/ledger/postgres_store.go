package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jjohngrey/double-entry-ledger/internal/db"
	"github.com/shopspring/decimal"
)

type PostgresStore struct {
	db *sql.DB
}

func NewPostgresStore(sqlDB *sql.DB) *PostgresStore {
	return &PostgresStore{db: sqlDB}
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

	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	q := db.New(tx)

	if idempotencyKey != "" {
		txID, err := q.GetIdempotencyKey(ctx, idempotencyKey)
		if err == nil {
			existing, err := s.buildTransaction(q, ctx, txID)
			if err != nil {
				return nil, false, err
			}
			return existing, true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("check idempotency key: %w", err)
		}
	}

	txID, err := q.CreateTransaction(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("create transaction row: %w", err)
	}

	var ledgerEntries []Entry
	for _, e := range entries {
		accountID, err := uuid.Parse(e.AccountID)
		if err != nil {
			return nil, false, fmt.Errorf("invalid account ID %s: %w", e.AccountID, err)
		}

		entryID, err := q.CreateEntry(ctx, db.CreateEntryParams{
			AccountID:     accountID,
			TransactionID: txID,
			Credit:        e.Credit.String(),
			Debit:         e.Debit.String(),
		})
		if err != nil {
			return nil, false, fmt.Errorf("create entry: %w", err)
		}

		_, err = q.UpdateAccountBalance(ctx, db.UpdateAccountBalanceParams{
			ID:        accountID,
			Balance:   e.Debit.String(),
			Balance_2: e.Credit.String(),
		})
		if err != nil {
			return nil, false, fmt.Errorf("update account balance: %w", err)
		}

		ledgerEntries = append(ledgerEntries, Entry{
			ID:            entryID.String(),
			AccountID:     e.AccountID,
			TransactionID: txID.String(),
			Credit:        e.Credit,
			Debit:         e.Debit,
		})
	}

	if idempotencyKey != "" {
		if err := q.CreateIdempotencyKey(ctx, db.CreateIdempotencyKeyParams{
			Key:           idempotencyKey,
			TransactionID: txID,
		}); err != nil {
			return nil, false, fmt.Errorf("store idempotency key: %w", err)
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
