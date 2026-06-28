package ledger

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"
)

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

func TestPostgresStore_CreateTransaction_SerializablePreventsOverdraft(t *testing.T) {
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
		TRUNCATE idempotency_keys, entries, transactions, accounts
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
	store.afterBalanceCheck = func(accountID uuid.UUID) {
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
