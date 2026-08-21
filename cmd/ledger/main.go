package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	httphandlers "github.com/jjohngrey/double-entry-ledger/internal/http"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer sqlDB.Close()
	if err := configureDBPool(sqlDB); err != nil {
		fmt.Fprintf(os.Stderr, "configure db pool: %v\n", err)
		os.Exit(1)
	}

	if err := runMigrations(sqlDB); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		os.Exit(1)
	}

	store := ledger.NewPostgresStore(sqlDB)
	postingBatchSize, err := positiveEnvInt("POSTING_BATCH_SIZE", 32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure posting batch size: %v\n", err)
		os.Exit(1)
	}
	postingBatchWait, err := envDuration("POSTING_BATCH_WAIT", 500*time.Microsecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure posting batch wait: %v\n", err)
		os.Exit(1)
	}
	postingBatchWorkers, err := positiveEnvInt("POSTING_BATCH_WORKERS", 4)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure posting batch workers: %v\n", err)
		os.Exit(1)
	}
	postingStore, err := ledger.NewBatchingPostgresStore(store, postingBatchSize, postingBatchWait, postingBatchWorkers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure posting batcher: %v\n", err)
		os.Exit(1)
	}
	defer postingStore.Close()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	natsConn, publisher, err := ledger.NewJetStreamPublisher(natsURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect JetStream: %v\n", err)
		os.Exit(1)
	}
	defer natsConn.Close()
	workerBatch, err := envInt("WORKER_BATCH_SIZE", 100)
	if err != nil || workerBatch == 0 {
		fmt.Fprintf(os.Stderr, "configure worker batch: WORKER_BATCH_SIZE must be positive\n")
		os.Exit(1)
	}
	workerIdleDelay, err := envDuration("WORKER_IDLE_DELAY", 10*time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure worker idle delay: %v\n", err)
		os.Exit(1)
	}
	transferWorkers, err := positiveEnvInt("TRANSFER_WORKERS", 16)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure transfer workers: %v\n", err)
		os.Exit(1)
	}
	publisherWorkers, err := positiveEnvInt("PUBLISHER_WORKERS", 16)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure publisher workers: %v\n", err)
		os.Exit(1)
	}
	aggregateWorkers, err := positiveEnvInt("AGGREGATE_WORKERS", 4)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure aggregate workers: %v\n", err)
		os.Exit(1)
	}
	aggregateFetchBatch, err := positiveEnvInt("AGGREGATE_FETCH_BATCH", 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure aggregate fetch batch: %v\n", err)
		os.Exit(1)
	}
	for i := 0; i < transferWorkers; i++ {
		startBackgroundWorker(fmt.Sprintf("transfer recovery worker %d", i+1), workerIdleDelay, func() (int, error) {
			return store.ProcessOutbox(workerBatch)
		})
	}
	for i := 0; i < publisherWorkers; i++ {
		startBackgroundWorker(fmt.Sprintf("transaction event publisher %d", i+1), workerIdleDelay, func() (int, error) {
			return store.PublishCommittedEvents(context.Background(), publisher, workerBatch)
		})
	}
	go func() {
		if err := ledger.RunAggregateConsumerConcurrent(context.Background(), publisher.JetStream(), store, aggregateWorkers, aggregateFetchBatch); err != nil {
			fmt.Fprintf(os.Stderr, "aggregate consumer: %v\n", err)
		}
	}()

	r := chi.NewRouter()
	requestLog, err := envBool("HTTP_REQUEST_LOG", true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure request logging: %v\n", err)
		os.Exit(1)
	}
	if requestLog {
		r.Use(middleware.Logger)
	}

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	})

	r.Post("/accounts", httphandlers.CreateAccountHandler(store))
	r.Get("/accounts/{account_id}/transactions", httphandlers.GetAccountEntriesHandler(store))
	r.Get("/accounts/{account_id}/aggregates", httphandlers.GetAccountAggregatesHandler(store))
	r.Get("/ledgers/{ledger_id}/aggregates/daily", httphandlers.GetLedgerDailyAggregatesHandler(store))
	r.Get("/balance", httphandlers.GetBalanceHandler(store))
	r.Post("/transactions", httphandlers.CreateTransactionHandler(postingStore))
	r.Get("/transactions/{transaction_id}/projection-status", httphandlers.GetTransactionProjectionStatusHandler(store))
	r.Post("/transfers", httphandlers.CreateTransferHandler(store))
	r.Get("/transfers/{transfer_id}", httphandlers.GetTransferHandler(store))

	if pprofAddr := os.Getenv("PPROF_ADDR"); pprofAddr != "" {
		go serveDiagnostics(pprofAddr, sqlDB)
	}

	httpAddr := os.Getenv("HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":3000"
	}
	fmt.Printf("Starting server on %s\n", httpAddr)
	if err := http.ListenAndServe(httpAddr, r); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
	}
}

func configureDBPool(sqlDB *sql.DB) error {
	maxOpen, err := envInt("DB_MAX_OPEN_CONNS", 0)
	if err != nil {
		return err
	}
	maxIdle, err := envInt("DB_MAX_IDLE_CONNS", 2)
	if err != nil {
		return err
	}
	maxLifetime, err := envDuration("DB_CONN_MAX_LIFETIME", 0)
	if err != nil {
		return err
	}
	maxIdleTime, err := envDuration("DB_CONN_MAX_IDLE_TIME", 0)
	if err != nil {
		return err
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(maxLifetime)
	sqlDB.SetConnMaxIdleTime(maxIdleTime)
	return nil
}

func envInt(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, raw)
	}
	return value, nil
}

func positiveEnvInt(name string, fallback int) (int, error) {
	value, err := envInt(name, fallback)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative Go duration, got %q", name, raw)
	}
	return value, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return value, nil
}

func serveDiagnostics(addr string, sqlDB *sql.DB) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
	mux.HandleFunc("/debug/db-stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sqlDB.Stats()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	fmt.Printf("Starting diagnostics on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "diagnostics: %v\n", err)
	}
}

func startBackgroundWorker(name string, idleDelay time.Duration, work func() (int, error)) {
	go func() {
		for {
			processed, err := work()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
				time.Sleep(idleDelay)
				continue
			}
			if processed == 0 {
				time.Sleep(idleDelay)
			}
		}
	}()
}

func runMigrations(sqlDB *sql.DB) error {
	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithDatabaseInstance("file://migrations", "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return err
	}
	return nil
}
