package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
	"github.com/nats-io/nats.go"
	"github.com/shopspring/decimal"
)

const benchmarkAmount = "1.00"

type accountPair struct {
	LedgerID        string `json:"ledger_id"`
	DebitAccountID  string `json:"debit_account_id"`
	CreditAccountID string `json:"credit_account_id"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	TransactionID   string `json:"transaction_id,omitempty"`
}

type transferPair struct {
	SourceLedgerID       string `json:"source_ledger_id"`
	SourceAccountID      string `json:"source_account_id"`
	DestinationLedgerID  string `json:"destination_ledger_id"`
	DestinationAccountID string `json:"destination_account_id"`
}

type seedFile struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Database      string    `json:"database"`
	Dataset       struct {
		AccountPairs        int   `json:"account_pairs"`
		HistoryTransactions int   `json:"history_transactions"`
		Accounts            int64 `json:"accounts"`
		Transactions        int64 `json:"transactions"`
		Entries             int64 `json:"entries"`
		DatabaseBytes       int64 `json:"database_bytes"`
	} `json:"dataset"`
	Scenarios struct {
		NormalBalancedPosting      []accountPair  `json:"normal_balanced_posting"`
		IdempotentDuplicatePosting []accountPair  `json:"idempotent_duplicate_posting"`
		CrossLedgerTransfer        []transferPair `json:"cross_ledger_transfer"`
		AggregateEventIngestion    []accountPair  `json:"aggregate_event_ingestion"`
	} `json:"scenarios"`
}

func main() {
	var output string
	var migrationsPath string
	var pairs int
	var history int
	var allowUnsafeReset bool
	var resetStream bool
	flag.StringVar(&output, "output", "benchmark/data.json", "seed metadata output path")
	flag.StringVar(&migrationsPath, "migrations", "migrations", "migration directory")
	flag.IntVar(&pairs, "account-pairs", envInt("BENCHMARK_ACCOUNT_PAIRS", 512), "isolated account pairs per scenario")
	flag.IntVar(&history, "history-transactions", envInt("BENCHMARK_HISTORY_TRANSACTIONS", 100000), "background transactions to seed")
	flag.BoolVar(&allowUnsafeReset, "allow-unsafe-reset", false, "allow truncating a database whose name does not contain benchmark")
	flag.BoolVar(&resetStream, "reset-stream", true, "delete the benchmark JetStream stream before seeding")
	flag.Parse()

	if pairs < 1 || history < 0 {
		fatalf("account-pairs must be positive and history-transactions must be non-negative")
	}
	dsn := os.Getenv("BENCHMARK_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fatalf("BENCHMARK_DATABASE_URL is required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fatalf("open database: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := waitForDatabase(ctx, db); err != nil {
		fatalf("connect database: %v", err)
	}

	var databaseName string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		fatalf("read database name: %v", err)
	}
	if !strings.Contains(strings.ToLower(databaseName), "benchmark") && !allowUnsafeReset && os.Getenv("BENCHMARK_ALLOW_UNSAFE_RESET") != "1" {
		fatalf("refusing to reset database %q: use a dedicated database whose name contains benchmark", databaseName)
	}
	if err := runMigrations(db, migrationsPath); err != nil {
		fatalf("migrate database: %v", err)
	}
	if resetStream {
		if err := resetJetStream(); err != nil {
			fatalf("reset JetStream: %v", err)
		}
	}

	result, err := seed(ctx, db, databaseName, pairs, history)
	if err != nil {
		fatalf("seed benchmark: %v", err)
	}
	if err := writeSeedFile(output, result); err != nil {
		fatalf("write seed metadata: %v", err)
	}
	fmt.Printf("seeded %s: %d accounts, %d transactions, %d entries (%s)\n", databaseName, result.Dataset.Accounts, result.Dataset.Transactions, result.Dataset.Entries, output)
}

func seed(ctx context.Context, db *sql.DB, databaseName string, pairs, history int) (seedFile, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return seedFile{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		TRUNCATE processed_events, daily_account_aggregates, daily_ledger_aggregates,
		outbox_events, saga_steps, sagas, idempotency_keys, entries, transactions, accounts
		RESTART IDENTITY CASCADE
	`); err != nil {
		return seedFile{}, fmt.Errorf("reset tables: %w", err)
	}

	result := seedFile{SchemaVersion: 1, GeneratedAt: time.Now().UTC(), Database: databaseName}
	result.Dataset.AccountPairs = pairs
	result.Dataset.HistoryTransactions = history
	insertAccount, err := tx.PrepareContext(ctx, `INSERT INTO accounts (id,ledger_id,name,type,balance) VALUES ($1,$2,$3,$4,$5)`)
	if err != nil {
		return seedFile{}, err
	}
	defer insertAccount.Close()

	for i := 0; i < pairs; i++ {
		normalLedger := benchmarkUUID("normal", i, "ledger")
		normalDebit := benchmarkUUID("normal", i, "debit")
		normalCredit := benchmarkUUID("normal", i, "credit")
		if err := insertAccounts(ctx, insertAccount,
			accountSeed{normalDebit, normalLedger, fmt.Sprintf("bench-normal-debit-%06d", i), "asset", "0"},
			accountSeed{normalCredit, normalLedger, fmt.Sprintf("bench-normal-credit-%06d", i), "revenue", "0"},
		); err != nil {
			return seedFile{}, err
		}
		result.Scenarios.NormalBalancedPosting = append(result.Scenarios.NormalBalancedPosting, accountPair{
			LedgerID: normalLedger.String(), DebitAccountID: normalDebit.String(), CreditAccountID: normalCredit.String(),
		})

		duplicateLedger := benchmarkUUID("duplicate", i, "ledger")
		duplicateDebit := benchmarkUUID("duplicate", i, "debit")
		duplicateCredit := benchmarkUUID("duplicate", i, "credit")
		duplicateTx := benchmarkUUID("duplicate", i, "transaction")
		duplicateKey := fmt.Sprintf("benchmark-duplicate-%06d", i)
		if err := insertAccounts(ctx, insertAccount,
			accountSeed{duplicateDebit, duplicateLedger, fmt.Sprintf("bench-duplicate-debit-%06d", i), "asset", benchmarkAmount},
			accountSeed{duplicateCredit, duplicateLedger, fmt.Sprintf("bench-duplicate-credit-%06d", i), "revenue", "-1.00"},
		); err != nil {
			return seedFile{}, err
		}
		entries := []ledger.EntryRequest{
			{AccountID: duplicateDebit.String(), Debit: decimal.RequireFromString(benchmarkAmount)},
			{AccountID: duplicateCredit.String(), Credit: decimal.RequireFromString(benchmarkAmount)},
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transactions (id,ledger_id) VALUES ($1,$2)`, duplicateTx, duplicateLedger); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries (id,account_id,transaction_id,credit,debit) VALUES ($1,$2,$3,0,$4),($5,$6,$3,$4,0)`,
			benchmarkUUID("duplicate", i, "debit-entry"), duplicateDebit, duplicateTx, benchmarkAmount,
			benchmarkUUID("duplicate", i, "credit-entry"), duplicateCredit); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (key,transaction_id,request_checksum,status) VALUES ($1,$2,$3,'completed')`, duplicateKey, duplicateTx, ledger.TransactionRequestChecksum("", entries)); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_events (id,transaction_id,event_type,status,available_at,processed_at) VALUES ($1,$2,'transaction_posted','processed',NOW(),NOW())`, benchmarkUUID("duplicate", i, "outbox"), duplicateTx); err != nil {
			return seedFile{}, err
		}
		result.Scenarios.IdempotentDuplicatePosting = append(result.Scenarios.IdempotentDuplicatePosting, accountPair{
			LedgerID: duplicateLedger.String(), DebitAccountID: duplicateDebit.String(), CreditAccountID: duplicateCredit.String(), IdempotencyKey: duplicateKey, TransactionID: duplicateTx.String(),
		})

		sourceLedger := benchmarkUUID("transfer", i, "source-ledger")
		destinationLedger := benchmarkUUID("transfer", i, "destination-ledger")
		source := benchmarkUUID("transfer", i, "source")
		sourceOffset := benchmarkUUID("transfer", i, "source-offset")
		destination := benchmarkUUID("transfer", i, "destination")
		sourceClearing := benchmarkUUID("transfer", i, "source-clearing")
		destinationClearing := benchmarkUUID("transfer", i, "destination-clearing")
		fundingTransaction := benchmarkUUID("transfer", i, "funding-transaction")
		if err := insertAccounts(ctx, insertAccount,
			accountSeed{source, sourceLedger, fmt.Sprintf("bench-transfer-source-%06d", i), "asset", "1000000000.00"},
			accountSeed{sourceOffset, sourceLedger, fmt.Sprintf("bench-transfer-offset-%06d", i), "revenue", "-1000000000.00"},
			accountSeed{sourceClearing, sourceLedger, "__transfer_clearing__", "asset", "0"},
			accountSeed{destination, destinationLedger, fmt.Sprintf("bench-transfer-destination-%06d", i), "asset", "0"},
			accountSeed{destinationClearing, destinationLedger, "__transfer_clearing__", "asset", "0"},
		); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO transactions (id,ledger_id) VALUES ($1,$2)`, fundingTransaction, sourceLedger); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries (id,account_id,transaction_id,credit,debit) VALUES ($1,$2,$3,0,1000000000),($4,$5,$3,1000000000,0)`,
			benchmarkUUID("transfer", i, "funding-debit-entry"), source, fundingTransaction,
			benchmarkUUID("transfer", i, "funding-credit-entry"), sourceOffset); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_events (id,transaction_id,event_type,status,available_at,processed_at) VALUES ($1,$2,'transaction_posted','processed',NOW(),NOW())`, benchmarkUUID("transfer", i, "funding-outbox"), fundingTransaction); err != nil {
			return seedFile{}, err
		}
		result.Scenarios.CrossLedgerTransfer = append(result.Scenarios.CrossLedgerTransfer, transferPair{
			SourceLedgerID: sourceLedger.String(), SourceAccountID: source.String(), DestinationLedgerID: destinationLedger.String(), DestinationAccountID: destination.String(),
		})

		aggregateLedger := benchmarkUUID("aggregate", i, "ledger")
		aggregateDebit := benchmarkUUID("aggregate", i, "debit")
		aggregateCredit := benchmarkUUID("aggregate", i, "credit")
		if err := insertAccounts(ctx, insertAccount,
			accountSeed{aggregateDebit, aggregateLedger, fmt.Sprintf("bench-aggregate-debit-%06d", i), "asset", "0"},
			accountSeed{aggregateCredit, aggregateLedger, fmt.Sprintf("bench-aggregate-credit-%06d", i), "revenue", "0"},
		); err != nil {
			return seedFile{}, err
		}
		result.Scenarios.AggregateEventIngestion = append(result.Scenarios.AggregateEventIngestion, accountPair{
			LedgerID: aggregateLedger.String(), DebitAccountID: aggregateDebit.String(), CreditAccountID: aggregateCredit.String(),
		})
	}

	if history > 0 {
		historyLedger := benchmarkUUID("history", 0, "ledger")
		historyDebit := benchmarkUUID("history", 0, "debit")
		historyCredit := benchmarkUUID("history", 0, "credit")
		amount := fmt.Sprintf("%d.00", history)
		if err := insertAccounts(ctx, insertAccount,
			accountSeed{historyDebit, historyLedger, "bench-history-debit", "asset", amount},
			accountSeed{historyCredit, historyLedger, "bench-history-credit", "revenue", "-" + amount},
		); err != nil {
			return seedFile{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			WITH inserted AS (
				INSERT INTO transactions (ledger_id)
				SELECT $1::uuid FROM generate_series(1,$2)
				RETURNING id
			)
			INSERT INTO entries (account_id,transaction_id,credit,debit)
			SELECT $3::uuid,id,0,1 FROM inserted
			UNION ALL
			SELECT $4::uuid,id,1,0 FROM inserted
		`, historyLedger, history, historyDebit, historyCredit); err != nil {
			return seedFile{}, fmt.Errorf("seed history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox_events (transaction_id,event_type,status,available_at,processed_at)
			SELECT id,'transaction_posted','processed',NOW(),NOW()
			FROM transactions
			WHERE ledger_id=$1
		`, historyLedger); err != nil {
			return seedFile{}, fmt.Errorf("seed history outbox: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return seedFile{}, err
	}
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		return seedFile{}, fmt.Errorf("analyze: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM accounts),(SELECT COUNT(*) FROM transactions),(SELECT COUNT(*) FROM entries),pg_database_size(current_database())`).Scan(
		&result.Dataset.Accounts, &result.Dataset.Transactions, &result.Dataset.Entries, &result.Dataset.DatabaseBytes,
	); err != nil {
		return seedFile{}, err
	}
	return result, nil
}

type accountSeed struct {
	id       uuid.UUID
	ledgerID uuid.UUID
	name     string
	typeName string
	balance  string
}

func insertAccounts(ctx context.Context, statement *sql.Stmt, accounts ...accountSeed) error {
	for _, account := range accounts {
		if _, err := statement.ExecContext(ctx, account.id, account.ledgerID, account.name, account.typeName, account.balance); err != nil {
			return fmt.Errorf("insert account %s: %w", account.name, err)
		}
	}
	return nil
}

func benchmarkUUID(scenario string, index int, purpose string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("double-entry-ledger/benchmark/%s/%d/%s", scenario, index, purpose)))
}

func runMigrations(db *sql.DB, path string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithDatabaseInstance((&url.URL{Scheme: "file", Path: absPath}).String(), "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func resetJetStream() error {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	deadline := time.Now().Add(time.Minute)
	var lastErr error
	for {
		nc, err := nats.Connect(natsURL, nats.Timeout(5*time.Second))
		if err == nil {
			js, jetStreamErr := nc.JetStream()
			if jetStreamErr == nil {
				deleteErr := js.DeleteStream(ledger.LedgerStreamName)
				nc.Close()
				if deleteErr == nil || errors.Is(deleteErr, nats.ErrStreamNotFound) {
					return nil
				}
				lastErr = deleteErr
			} else {
				lastErr = jetStreamErr
				nc.Close()
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(time.Second)
	}
}

func waitForDatabase(ctx context.Context, db *sql.DB) error {
	var lastErr error
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func writeSeedFile(path string, data seedFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

func envInt(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		fatalf("%s must be an integer, got %q", name, raw)
	}
	return value
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
