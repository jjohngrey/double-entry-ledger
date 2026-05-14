package main

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	httphandlers "github.com/jjohngrey/double-entry-ledger/internal/http"
	"github.com/jjohngrey/double-entry-ledger/internal/ledger"
)

func main() {
	// initialize in-memory data stores
	store := ledger.NewStore()

	// initialize the router
	r := chi.NewRouter()

	// set up logger
	r.Use(middleware.Logger)

	// health check endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	})

	// create account
	r.Post("/accounts", httphandlers.CreateAccountHandler(store))

	// get balance
	r.Get("/balance", httphandlers.GetBalanceHandler(store))

	// server startup message
	fmt.Println("Starting server on :3000")

	// error handling for server startup
	if err := http.ListenAndServe(":3000", r); err != nil {
		fmt.Printf("Error starting server: %v\n", err)
	}
}
