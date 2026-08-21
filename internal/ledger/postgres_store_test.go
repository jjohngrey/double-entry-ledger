package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"
)

func TestPostgresStore_CreateTransaction_RetriesSerializationFailures(t *testing.T) {
	var calls int
	store := &PostgresStore{
		sleep: func(time.Duration) {},
		transactionAttempt: func(string, []EntryRequest) (*Transaction, bool, error) {
			calls++
			if calls <= 2 {
				return nil, false, fmt.Errorf("commit: %w", &pgconn.PgError{Code: "40001", Message: "could not serialize access"})
			}
			return &Transaction{ID: "posted"}, false, nil
		},
	}

	transaction, existed, err := store.CreateTransaction("", validTransactionEntries())
	if err != nil {
		t.Fatalf("create transaction: %v", err)
	}
	if existed || transaction.ID != "posted" || calls != 3 {
		t.Fatalf("expected third attempt to post, got transaction=%#v existed=%t calls=%d", transaction, existed, calls)
	}
	if metrics := store.RetryMetrics(); metrics.Attempts != 2 || metrics.Exhausted != 0 {
		t.Fatalf("unexpected retry metrics: %#v", metrics)
	}
}

func TestPostgresStore_CreateTransaction_SerializationRetryExhaustionIsControlled(t *testing.T) {
	store := &PostgresStore{
		sleep: func(time.Duration) {},
		transactionAttempt: func(string, []EntryRequest) (*Transaction, bool, error) {
			return nil, false, fmt.Errorf("update account balance: %w", &pgconn.PgError{Code: "40001", Message: "raw postgres serialization detail"})
		},
	}

	_, _, err := store.CreateTransaction("", validTransactionEntries())
	if !errors.Is(err, ErrTransactionRetryExhausted) {
		t.Fatalf("expected controlled exhaustion error, got %v", err)
	}
	if strings.Contains(err.Error(), "raw postgres") {
		t.Fatalf("controlled error exposed PostgreSQL text: %q", err)
	}
	if metrics := store.RetryMetrics(); metrics.Attempts != maxSerializationRetries || metrics.Exhausted != 1 {
		t.Fatalf("unexpected retry metrics: %#v", metrics)
	}
}

func validTransactionEntries() []EntryRequest {
	return []EntryRequest{
		{AccountID: uuid.NewString(), Debit: decimal.NewFromInt(1)},
		{AccountID: uuid.NewString(), Credit: decimal.NewFromInt(1)},
	}
}

func TestPostgresStore_CreateTransaction_ReadCommittedRaceDemo(t *testing.T) {
	t.Skip("demonstration test: unskip to see the old read-committed race produce a negative balance")

	sqlDB := openPostgresTestDB(t)
	store := newPostgresStoreWithTxOptions(sqlDB, nil)
	cash, expense := seedPostgresBalance(t, store)
	withdrawal := withdrawalEntries(cash.ID, expense.ID)

	successes, _, finalBalance := runConcurrentWithdrawals(t, store, cash.ID, withdrawal)

	if successes == 2 && finalBalance.Equal(decimal.NewFromInt(-80)) {
		t.Fatalf("race reproduced: both withdrawals succeeded and final balance is %s", finalBalance)
	}
}

func TestPostgresStore_CreateTransaction_RowLocksPreventOverdraft(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	cash, expense := seedPostgresBalance(t, store)
	withdrawal := withdrawalEntries(cash.ID, expense.ID)

	successes, errors, finalBalance := runConcurrentWithdrawals(t, store, cash.ID, withdrawal)

	if successes != 1 {
		t.Fatalf("expected exactly one successful withdrawal, got %d", successes)
	}
	if errors != 1 {
		t.Fatalf("expected exactly one failed withdrawal, got %d", errors)
	}
	if !finalBalance.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("expected final balance of 10, got %s", finalBalance)
	}
	if got := store.RetryMetrics().Attempts; got != 0 {
		t.Fatalf("expected row locking without a serialization retry, got %d", got)
	}
}

func TestPostgresStore_CreateTransaction_RetriesContentionWhenFundsRemain(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	store.sleep = func(time.Duration) {}
	cash, expense := seedPostgresBalance(t, store)
	withdrawal := []EntryRequest{
		{AccountID: expense.ID, Debit: decimal.NewFromInt(40), Credit: decimal.Zero},
		{AccountID: cash.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(40)},
	}

	successes, failures, finalBalance := runConcurrentWithdrawals(t, store, cash.ID, withdrawal)

	if successes != 2 || failures != 0 {
		t.Fatalf("expected both withdrawals to succeed after retry, got successes=%d failures=%d", successes, failures)
	}
	if !finalBalance.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("expected final balance of 20, got %s", finalBalance)
	}
	if got := store.RetryMetrics().Attempts; got != 0 {
		t.Fatalf("expected row locking without a serialization retry, got %d", got)
	}
}

func TestPostgresStore_CreateTransaction_IdempotencyIsAtomic(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	store.sleep = func(time.Duration) {}
	cash, expense := seedPostgresBalance(t, store)
	entries := []EntryRequest{
		{AccountID: expense.ID, Debit: decimal.NewFromInt(10)},
		{AccountID: cash.ID, Credit: decimal.NewFromInt(10)},
	}

	start := make(chan struct{})
	type result struct {
		transaction *Transaction
		existed     bool
		err         error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			transaction, existed, err := store.CreateTransaction("same-key", entries)
			results <- result{transaction: transaction, existed: existed, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var transactionID string
	newCount, existingCount := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent posting: %v", result.err)
		}
		if transactionID != "" && transactionID != result.transaction.ID {
			t.Fatalf("expected one transaction, got %s and %s", transactionID, result.transaction.ID)
		}
		transactionID = result.transaction.ID
		if result.existed {
			existingCount++
		} else {
			newCount++
		}
	}
	if newCount != 1 || existingCount != 1 {
		t.Fatalf("expected one new and one existing response, got new=%d existing=%d", newCount, existingCount)
	}
	var transactionCount, entryCount, keyCount int
	if err := sqlDB.QueryRow("SELECT COUNT(*) FROM transactions").Scan(&transactionCount); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRow("SELECT COUNT(*) FROM entries").Scan(&entryCount); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRow("SELECT COUNT(*) FROM idempotency_keys WHERE key = 'same-key'").Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if transactionCount != 2 || entryCount != 4 || keyCount != 1 {
		t.Fatalf("unexpected persisted counts: transactions=%d entries=%d keys=%d", transactionCount, entryCount, keyCount)
	}
}

func TestPostgresStore_CreateTransaction_FailedRequestDoesNotReserveKey(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	cash, expense := seedPostgresBalance(t, store)

	_, _, err := store.CreateTransaction("retry-after-failure", []EntryRequest{
		{AccountID: expense.ID, Debit: decimal.NewFromInt(110)},
		{AccountID: cash.ID, Credit: decimal.NewFromInt(110)},
	})
	if err == nil {
		t.Fatal("expected insufficient-funds request to fail")
	}
	transaction, existed, err := store.CreateTransaction("retry-after-failure", []EntryRequest{
		{AccountID: expense.ID, Debit: decimal.NewFromInt(10)},
		{AccountID: cash.ID, Credit: decimal.NewFromInt(10)},
	})
	if err != nil || existed || transaction == nil {
		t.Fatalf("retry after failed request: transaction=%#v existed=%t err=%v", transaction, existed, err)
	}
}

func TestPostgresStore_CreateTransaction_BatchesMoreThanTwoEntries(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	cash, err := store.CreateAccount("Batch cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	expense, err := store.CreateAccount("Batch expense", ExpenseType)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := store.CreateAccount("Batch revenue", RevenueType)
	if err != nil {
		t.Fatal(err)
	}

	transaction, existed, err := store.CreateTransaction("batch-more-than-two", []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(60)},
		{AccountID: expense.ID, Debit: decimal.NewFromInt(40)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(100)},
	})
	if err != nil || existed {
		t.Fatalf("create transaction: existed=%v err=%v", existed, err)
	}
	if len(transaction.Entries) != 3 {
		t.Fatalf("entry count=%d, want 3", len(transaction.Entries))
	}
	for accountID, expected := range map[string]int64{cash.ID: 60, expense.ID: 40, revenue.ID: 100} {
		balance, err := store.GetBalance(accountID)
		if err != nil || !balance.Equal(decimal.NewFromInt(expected)) {
			t.Fatalf("balance account=%s got=%s want=%d err=%v", accountID, balance, expected, err)
		}
	}
}

func TestPostgresStore_CreateTransaction_BatchesRepeatedAccountIDs(t *testing.T) {
	sqlDB := openPostgresTestDB(t)
	store := NewPostgresStore(sqlDB)
	cash, err := store.CreateAccount("Repeated cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	revenue, err := store.CreateAccount("Repeated revenue", RevenueType)
	if err != nil {
		t.Fatal(err)
	}

	request := []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(30)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(20)},
		{AccountID: cash.ID, Debit: decimal.NewFromInt(20)},
		{AccountID: revenue.ID, Credit: decimal.NewFromInt(30)},
	}
	transaction, existed, err := store.CreateTransaction("batch-repeated", request)
	if err != nil || existed {
		t.Fatalf("create transaction: existed=%v err=%v", existed, err)
	}
	if len(transaction.Entries) != len(request) {
		t.Fatalf("entry count=%d, want %d", len(transaction.Entries), len(request))
	}
	for i, entry := range transaction.Entries {
		if entry.AccountID != request[i].AccountID || !entry.Debit.Equal(request[i].Debit) || !entry.Credit.Equal(request[i].Credit) {
			t.Fatalf("entry %d did not preserve request order: got=%#v request=%#v", i, entry, request[i])
		}
	}
	for _, accountID := range []string{cash.ID, revenue.ID} {
		balance, err := store.GetBalance(accountID)
		if err != nil || !balance.Equal(decimal.NewFromInt(50)) {
			t.Fatalf("balance account=%s got=%s want=50 err=%v", accountID, balance, err)
		}
	}
}

func openPostgresTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping Postgres integration test")
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		sqlDB.Close()
	})

	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	runPostgresTestMigrations(t, sqlDB)
	truncatePostgresTestData(t, sqlDB)

	return sqlDB
}

func runPostgresTestMigrations(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		t.Fatalf("create migrate driver: %v", err)
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find test path")
	}
	migrationsPath := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")

	m, err := migrate.NewWithDatabaseInstance(fmt.Sprintf("file://%s", migrationsPath), "postgres", driver)
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("run migrations: %v", err)
	}
}

func truncatePostgresTestData(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	_, err := sqlDB.Exec(`
		TRUNCATE processed_events, daily_account_aggregates, daily_ledger_aggregates,
		outbox_events, saga_steps, sagas, idempotency_keys, entries, transactions, accounts
		RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("truncate test data: %v", err)
	}
}

func seedPostgresBalance(t *testing.T, store *PostgresStore) (*Account, *Account) {
	t.Helper()

	cash, err := store.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatalf("create cash account: %v", err)
	}
	equity, err := store.CreateAccount("Owner Equity", EquityType)
	if err != nil {
		t.Fatalf("create equity account: %v", err)
	}
	expense, err := store.CreateAccount("Supplies", ExpenseType)
	if err != nil {
		t.Fatalf("create expense account: %v", err)
	}

	_, _, err = store.CreateTransaction("", []EntryRequest{
		{AccountID: cash.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		{AccountID: equity.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
	})
	if err != nil {
		t.Fatalf("seed opening balance: %v", err)
	}

	return cash, expense
}

func withdrawalEntries(cashID, expenseID string) []EntryRequest {
	return []EntryRequest{
		{AccountID: expenseID, Debit: decimal.NewFromInt(90), Credit: decimal.Zero},
		{AccountID: cashID, Debit: decimal.Zero, Credit: decimal.NewFromInt(90)},
	}
}

func runConcurrentWithdrawals(t *testing.T, store *PostgresStore, guardedAccountID string, entries []EntryRequest) (int, int, decimal.Decimal) {
	t.Helper()

	guardedID, err := uuid.Parse(guardedAccountID)
	if err != nil {
		t.Fatalf("parse guarded account ID: %v", err)
	}

	start := make(chan struct{})
	balanceChecked := make(chan struct{}, 2)
	release := make(chan struct{})
	store.beforeAccountLock = func(accountID uuid.UUID) {
		if accountID != guardedID {
			return
		}
		balanceChecked <- struct{}{}
		<-release
	}

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := store.CreateTransaction("", entries)
			errs <- err
		}()
	}

	close(start)
	released := false
	for i := 0; i < 2; i++ {
		select {
		case <-balanceChecked:
		case <-time.After(5 * time.Second):
			close(release)
			released = true
			wg.Wait()
			t.Fatalf("timed out waiting for withdrawal %d to pass balance check", i+1)
		}
	}
	if !released {
		close(release)
	}

	wg.Wait()
	close(errs)

	successes := 0
	errors := 0
	for err := range errs {
		if err != nil {
			errors++
			continue
		}
		successes++
	}

	finalBalance, err := store.GetBalance(guardedAccountID)
	if err != nil {
		t.Fatalf("get final balance: %v", err)
	}

	return successes, errors, finalBalance
}
