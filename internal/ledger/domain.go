package ledger

import (
	"time"

	"github.com/shopspring/decimal"
)

// REQUEST MODELS
type CreateAccountRequest struct {
	LedgerID string      `json:"ledger_id"`
	Name     string      `json:"name"`
	Type     AccountType `json:"type"`
}

type EntryRequest struct {
	AccountID string          `json:"account_id"`
	Credit    decimal.Decimal `json:"credit"`
	Debit     decimal.Decimal `json:"debit"`
}

type CreateTransactionRequest struct {
	Entries        []EntryRequest `json:"entries"`
	IdempotencyKey string         `json:"idempotency_key"`
}

// TransferRequest deliberately identifies both the ledger and account at each
// boundary.  The store rejects an account whose ledger does not match the
// supplied boundary, preventing a caller from smuggling an account across a
// ledger boundary by ID alone.
type TransferRequest struct {
	SourceLedgerID       string          `json:"source_ledger_id"`
	SourceAccountID      string          `json:"source_account_id"`
	DestinationLedgerID  string          `json:"destination_ledger_id"`
	DestinationAccountID string          `json:"destination_account_id"`
	Amount               decimal.Decimal `json:"amount"`
	IdempotencyKey       string          `json:"idempotency_key"`
}

// OPTIONAL PARAMETER MODELS
type GetAccountEntriesParams struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// RESPONSE MODELS
type ErrorResponse struct {
	Error string `json:"error"`
}

type GetAccountEntriesResponse struct {
	Entries        []Entry         `json:"entries"`
	RunningBalance decimal.Decimal `json:"running_balance"`
}

type GetBalanceHandlerResponse struct {
	Balance decimal.Decimal `json:"balance"`
}

type TransferStatus string

const (
	TransferPending     TransferStatus = "pending"
	TransferCompleted   TransferStatus = "completed"
	TransferCompensated TransferStatus = "compensated"
	TransferFailed      TransferStatus = "failed"
)

type TransferResponse struct {
	ID                   string          `json:"id"`
	SourceLedgerID       string          `json:"source_ledger_id"`
	SourceAccountID      string          `json:"source_account_id"`
	DestinationLedgerID  string          `json:"destination_ledger_id"`
	DestinationAccountID string          `json:"destination_account_id"`
	Amount               decimal.Decimal `json:"amount"`
	Status               TransferStatus  `json:"status"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
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
	ID       string          `json:"id"`
	LedgerID string          `json:"ledger_id"`
	Name     string          `json:"name"`
	Type     AccountType     `json:"type"`
	Balance  decimal.Decimal `json:"balance"`
}

type Entry struct {
	ID            string          `json:"id"`
	AccountID     string          `json:"account_id"`     // FK to Account
	TransactionID string          `json:"transaction_id"` // FK to Transaction
	Credit        decimal.Decimal `json:"credit"`
	Debit         decimal.Decimal `json:"debit"`
}

type Transaction struct {
	ID        string    `json:"id"`
	LedgerID  string    `json:"ledger_id"`
	Entries   []Entry   `json:"entries"`
	Timestamp time.Time `json:"timestamp"`
	// Invariant: sum(Debit across all entries) == sum(Credit across all entries)
}

// TransactionPostedEvent is the immutable payload placed in JetStream after
// its database transaction commits. EventID is the outbox row ID.
type TransactionPostedEvent struct {
	EventID       string    `json:"event_id"`
	Type          string    `json:"type"`
	TransactionID string    `json:"transaction_id"`
	LedgerID      string    `json:"ledger_id"`
	Timestamp     time.Time `json:"timestamp"`
	Entries       []Entry   `json:"entries"`
}

type DailyAggregate struct {
	Day    time.Time       `json:"day"`
	Debit  decimal.Decimal `json:"debit"`
	Credit decimal.Decimal `json:"credit"`
}

// AggregateResponse deliberately includes a projection cursor. Aggregates are
// eventually consistent with the write ledger, so callers can show freshness.
type AggregateResponse struct {
	Aggregates          []DailyAggregate `json:"aggregates"`
	ProjectionTimestamp time.Time        `json:"projection_timestamp"`
	LastEventID         string           `json:"last_event_id"`
}

// TransactionProjectionStatus reports whether one committed transaction event
// has been applied by a specific durable projection consumer. Projected is
// exact for consumers that insert their processed_events row in the same
// database transaction as their projection updates.
type TransactionProjectionStatus struct {
	TransactionID string `json:"transaction_id"`
	EventID       string `json:"event_id"`
	Consumer      string `json:"consumer"`
	Projected     bool   `json:"projected"`
}
