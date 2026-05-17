package ledger

import (
	"time"

	"github.com/shopspring/decimal"
)

// REQUEST MODELS
type CreateAccountRequest struct {
	Name string      `json:"name"`
	Type AccountType `json:"type"`
}

type EntryRequest struct {
	AccountID string          `json:"account_id"`
	Credit    decimal.Decimal `json:"credit"`
	Debit     decimal.Decimal `json:"debit"`
}

type CreateTransactionRequest struct {
	Entries []EntryRequest `json:"entries"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// ENUMERATIONS
type AccountType string

const (
	AssetType     AccountType = "asset"
	LiabilityType AccountType = "liability"
	EquityType    AccountType = "equity"
	RevenueType   AccountType = "revenue"
	ExpenseType   AccountType = "expense"
)

// MODELS
type Account struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Type    AccountType     `json:"type"`
	Balance decimal.Decimal `json:"balance"`
}

type Entry struct {
	ID        string          `json:"id"`
	AccountID string          `json:"account_id"`
	Credit    decimal.Decimal `json:"credit"`
	Debit     decimal.Decimal `json:"debit"`
}

type Transaction struct {
	ID        string    `json:"id"`
	Entries   []Entry   `json:"entries"`
	Timestamp time.Time `json:"timestamp"`
	// Invariant: sum(Debit across all entries) == sum(Credit across all entries)
}
