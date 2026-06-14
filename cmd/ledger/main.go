package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	httphandlers "github.com/jjohngrey/double-entry-ledger/internal/http"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
	_ "github.com/jackc/pgx/v5/stdlib"
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

	if err := runMigrations(sqlDB); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		os.Exit(1)
	}

	store := ledger.NewPostgresStore(sqlDB)

	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	})

	r.Post("/accounts", httphandlers.CreateAccountHandler(store))
	r.Get("/accounts/{account_id}/transactions", httphandlers.GetAccountEntriesHandler(store))
	r.Get("/balance", httphandlers.GetBalanceHandler(store))
	r.Post("/transactions", httphandlers.CreateTransactionHandler(store))

	fmt.Println("Starting server on :3000")
	if err := http.ListenAndServe(":3000", r); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
	}
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
