package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	poolConfig, err := loadDBPoolConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure db pool: %v\n", err)
		os.Exit(1)
	}
	dedicatedConfig, err := loadDedicatedPoolConfig(poolConfig.maxOpen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure dedicated db pools: %v\n", err)
		os.Exit(1)
	}
	projectionDSN := os.Getenv("PROJECTION_DATABASE_URL")
	if projectionDSN != "" && dedicatedConfig.aggregate > 0 {
		// The aggregate worker no longer consumes an OLTP session budget.
		dedicatedConfig.foreground += dedicatedConfig.aggregate
		dedicatedConfig.aggregate = 0
	}
	configureDBPool(sqlDB, dedicatedConfig.foreground, poolConfig)

	if err := runMigrations(sqlDB); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		os.Exit(1)
	}

	store := ledger.NewPostgresStore(sqlDB)
	transferWorkerStore, publisherStore, aggregateStore := store, store, store
	var projectionStatusReader interface {
		GetTransactionProjectionStatus(context.Context, string, string) (ledger.TransactionProjectionStatus, error)
		WaitForTransactionProjection(context.Context, string, string, time.Duration) (ledger.TransactionProjectionStatus, error)
	} = store
	diagnosticPools := map[string]*sql.DB{"foreground": sqlDB}
	if dedicatedConfig.enabled() {
		transferDB, err := openDedicatedDB(dsn, dedicatedConfig.transfer, poolConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open transfer db pool: %v\n", err)
			os.Exit(1)
		}
		defer transferDB.Close()
		publisherDB, err := openDedicatedDB(dsn, dedicatedConfig.publisher, poolConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open publisher db pool: %v\n", err)
			os.Exit(1)
		}
		defer publisherDB.Close()
		transferWorkerStore = store.WithDatabase(transferDB)
		publisherStore = store.WithDatabase(publisherDB)
		diagnosticPools["transfer"] = transferDB
		diagnosticPools["publisher"] = publisherDB
		if dedicatedConfig.aggregate > 0 {
			aggregateDB, err := openDedicatedDB(dsn, dedicatedConfig.aggregate, poolConfig)
			if err != nil {
				fmt.Fprintf(os.Stderr, "open aggregate db pool: %v\n", err)
				os.Exit(1)
			}
			defer aggregateDB.Close()
			aggregateStore = store.WithDatabase(aggregateDB)
			diagnosticPools["aggregate"] = aggregateDB
		}
	}
	if projectionDSN != "" {
		projectionMaxOpen, err := positiveEnvInt("PROJECTION_DB_MAX_OPEN_CONNS", 20)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure projection db pool: %v\n", err)
			os.Exit(1)
		}
		projectionDB, err := openDedicatedDB(projectionDSN, projectionMaxOpen, poolConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open projection db pool: %v\n", err)
			os.Exit(1)
		}
		defer projectionDB.Close()
		if err := runMigrationsAt(projectionDB, "projection-migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "projection migrations: %v\n", err)
			os.Exit(1)
		}
		projectionStore := store.WithDatabase(projectionDB)
		aggregateStore = projectionStore
		projectionStatusReader = ledger.NewProjectionStatusStore(store, projectionStore)
		diagnosticPools["projection"] = projectionDB
	}
	postingBatchSize, err := positiveEnvInt("POSTING_BATCH_SIZE", 64)
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
	publishAsyncMaxPending, err := positiveEnvInt("PUBLISH_ASYNC_MAX_PENDING", 256)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure NATS async publication: %v\n", err)
		os.Exit(1)
	}
	natsConn, publisher, err := ledger.NewJetStreamPublisher(natsURL, publishAsyncMaxPending)
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
	transferWorkers, err := positiveEnvInt("TRANSFER_WORKERS", 1)
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
			return transferWorkerStore.ProcessOutbox(workerBatch)
		})
	}
	startBackgroundWorker("transfer compensation worker", 100*time.Millisecond, func() (int, error) {
		return transferWorkerStore.ProcessCompensationOutbox(workerBatch)
	})
	for i := 0; i < publisherWorkers; i++ {
		startBackgroundWorker(fmt.Sprintf("transaction event publisher %d", i+1), workerIdleDelay, func() (int, error) {
			return publisherStore.PublishCommittedEvents(context.Background(), publisher, workerBatch)
		})
	}
	go func() {
		if err := ledger.RunAggregateConsumerConcurrent(context.Background(), publisher.JetStream(), aggregateStore, aggregateWorkers, aggregateFetchBatch); err != nil {
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
	r.Get("/accounts/{account_id}/aggregates", httphandlers.GetAccountAggregatesHandler(aggregateStore))
	r.Get("/ledgers/{ledger_id}/aggregates/daily", httphandlers.GetLedgerDailyAggregatesHandler(aggregateStore))
	r.Get("/balance", httphandlers.GetBalanceHandler(store))
	r.Post("/transactions", httphandlers.CreateTransactionHandler(postingStore))
	r.Get("/transactions/{transaction_id}/projection-status", httphandlers.GetTransactionProjectionStatusHandler(projectionStatusReader))
	r.Post("/transfers", httphandlers.CreateTransferHandler(store))
	r.Get("/transfers/{transfer_id}", httphandlers.GetTransferHandler(store))

	if pprofAddr := os.Getenv("PPROF_ADDR"); pprofAddr != "" {
		go serveDiagnostics(pprofAddr, diagnosticPools)
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

type dbPoolConfig struct {
	maxOpen     int
	maxIdle     int
	maxLifetime time.Duration
	maxIdleTime time.Duration
}

type dedicatedPoolConfig struct {
	foreground int
	transfer   int
	publisher  int
	aggregate  int
}

func (c dedicatedPoolConfig) enabled() bool {
	return c.transfer+c.publisher+c.aggregate > 0
}

func loadDBPoolConfig() (dbPoolConfig, error) {
	maxOpen, err := envInt("DB_MAX_OPEN_CONNS", 0)
	if err != nil {
		return dbPoolConfig{}, err
	}
	maxIdle, err := envInt("DB_MAX_IDLE_CONNS", 2)
	if err != nil {
		return dbPoolConfig{}, err
	}
	maxLifetime, err := envDuration("DB_CONN_MAX_LIFETIME", 0)
	if err != nil {
		return dbPoolConfig{}, err
	}
	maxIdleTime, err := envDuration("DB_CONN_MAX_IDLE_TIME", 0)
	if err != nil {
		return dbPoolConfig{}, err
	}
	return dbPoolConfig{maxOpen: maxOpen, maxIdle: maxIdle, maxLifetime: maxLifetime, maxIdleTime: maxIdleTime}, nil
}

func loadDedicatedPoolConfig(total int) (dedicatedPoolConfig, error) {
	transfer, err := envInt("DB_TRANSFER_POOL_CONNS", 0)
	if err != nil {
		return dedicatedPoolConfig{}, err
	}
	publisher, err := envInt("DB_PUBLISHER_POOL_CONNS", 0)
	if err != nil {
		return dedicatedPoolConfig{}, err
	}
	aggregate, err := envInt("DB_AGGREGATE_POOL_CONNS", 0)
	if err != nil {
		return dedicatedPoolConfig{}, err
	}
	background := transfer + publisher + aggregate
	if background == 0 {
		return dedicatedPoolConfig{foreground: total}, nil
	}
	if transfer == 0 || publisher == 0 || aggregate == 0 {
		return dedicatedPoolConfig{}, errors.New("dedicated transfer, publisher, and aggregate pools must all be positive")
	}
	if total == 0 || background >= total {
		return dedicatedPoolConfig{}, fmt.Errorf("dedicated pools require a finite DB_MAX_OPEN_CONNS greater than their %d-connection sum", background)
	}
	return dedicatedPoolConfig{foreground: total - background, transfer: transfer, publisher: publisher, aggregate: aggregate}, nil
}

func configureDBPool(sqlDB *sql.DB, maxOpen int, config dbPoolConfig) {
	maxIdle := min(config.maxIdle, maxOpen)
	if maxOpen == 0 {
		maxIdle = config.maxIdle
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(config.maxLifetime)
	sqlDB.SetConnMaxIdleTime(config.maxIdleTime)
}

func openDedicatedDB(dsn string, maxOpen int, config dbPoolConfig) (*sql.DB, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	configureDBPool(sqlDB, maxOpen, config)
	return sqlDB, nil
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

type diagnosticDBStats struct {
	sql.DBStats
	Pools map[string]sql.DBStats `json:"Pools"`
}

func serveDiagnostics(addr string, sqlDBs map[string]*sql.DB) {
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
		stats := diagnosticDBStats{Pools: make(map[string]sql.DBStats, len(sqlDBs))}
		for name, sqlDB := range sqlDBs {
			poolStats := sqlDB.Stats()
			stats.Pools[name] = poolStats
			stats.MaxOpenConnections += poolStats.MaxOpenConnections
			stats.OpenConnections += poolStats.OpenConnections
			stats.InUse += poolStats.InUse
			stats.Idle += poolStats.Idle
			stats.WaitCount += poolStats.WaitCount
			stats.WaitDuration += poolStats.WaitDuration
			stats.MaxIdleClosed += poolStats.MaxIdleClosed
			stats.MaxIdleTimeClosed += poolStats.MaxIdleTimeClosed
			stats.MaxLifetimeClosed += poolStats.MaxLifetimeClosed
		}
		if err := json.NewEncoder(w).Encode(stats); err != nil {
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
	return runMigrationsAt(sqlDB, "migrations")
}

func runMigrationsAt(sqlDB *sql.DB, migrationPath string) error {
	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationPath, "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return err
	}
	return nil
}
