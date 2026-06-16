package ledger

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Store interface {
	CreateAccount(name string, accType AccountType) (*Account, error)
	CreateTransaction(idempotencyKey string, entries []EntryRequest) (*Transaction, bool, error)
	GetBalance(accountID string) (decimal.Decimal, error)
	GetAccountEntries(accountID string, params GetAccountEntriesParams) (GetAccountEntriesResponse, error)
}

var _ Store = (*MemoryStore)(nil) // compile-time assertion that MemoryStore implements Store

type MemoryStore struct {
	mu              sync.RWMutex
	accounts        map[string]Account
	transactions    []Transaction
	idempotencyKeys map[string]string // idempotencyKey -> transactionID
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts:        make(map[string]Account),
		transactions:    []Transaction{},
		idempotencyKeys: make(map[string]string),
	}
}

// Public methods

func (s *MemoryStore) CreateAccount(name string, accType AccountType) (*Account, error) {
	if err := ValidateAccount(name, accType); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id := uuid.New().String()

	account := &Account{
		ID:      id,
		Name:    name,
		Type:    accType,
		Balance: decimal.Zero,
	}

	s.accounts[id] = *account
	return account, nil
}

func (s *MemoryStore) CreateTransaction(idempotencyKey string, entries []EntryRequest) (*Transaction, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if transactionID, exists := s.idempotencyKeys[idempotencyKey]; exists {
			transaction, err := s.getTransactionByID(transactionID)
			if err != nil {
				return nil, false, fmt.Errorf("idempotency key references missing transaction")
			}
			return transaction, true, nil
		}
	}

	transaction, err := s.createTransaction(entries)
	if err != nil {
		return nil, false, err
	}

	if idempotencyKey != "" {
		s.idempotencyKeys[idempotencyKey] = transaction.ID
	}

	return transaction, false, nil
}

func (s *MemoryStore) GetAccountEntries(accountID string, params GetAccountEntriesParams) (GetAccountEntriesResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sortedTransactions := make([]Transaction, len(s.transactions))
	copy(sortedTransactions, s.transactions)
	sort.Slice(sortedTransactions, func(i, j int) bool {
		return sortedTransactions[i].Timestamp.Before(sortedTransactions[j].Timestamp)
	})
	var entries []Entry
	var runningBalance = decimal.Zero
	for _, transaction := range sortedTransactions {
		if (!params.From.IsZero() && transaction.Timestamp.Before(params.From)) ||
			(!params.To.IsZero() && transaction.Timestamp.After(params.To)) {
			continue
		}
		for _, entry := range transaction.Entries {
			if entry.AccountID == accountID {
				entries = append(entries, entry)
				runningBalance = runningBalance.Add(entry.Debit).Sub(entry.Credit)
			}
		}
	}

	res := GetAccountEntriesResponse{
		Entries:        entries,
		RunningBalance: runningBalance,
	}

	return res, nil
}

func (s *MemoryStore) GetBalance(accountID string) (decimal.Decimal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[accountID]
	if !exists {
		return decimal.Zero, fmt.Errorf("Account with ID %s not found", accountID)
	}

	if account.Type == LiabilityType || account.Type == EquityType || account.Type == RevenueType {
		return account.Balance.Neg(), nil
	}
	return account.Balance, nil
}

// Private methods
func (s *MemoryStore) createTransaction(entries []EntryRequest) (*Transaction, error) {
	if err := ValidateTransaction(entries); err != nil {
		return nil, err
	}

	transactionID := uuid.New().String()
	sec := time.Now()

	var transactionEntries []Entry

	for _, entry := range entries {
		_, exists := s.accounts[entry.AccountID]
		if !exists {
			return nil, fmt.Errorf("Account with ID %s not found", entry.AccountID)
		}
	}

	for _, entry := range entries {
		account := s.accounts[entry.AccountID]

		entryID := uuid.New().String()
		newEntry := &Entry{
			ID:            entryID,
			AccountID:     account.ID,
			TransactionID: transactionID,
			Credit:        entry.Credit,
			Debit:         entry.Debit,
		}

		transactionEntries = append(transactionEntries, *newEntry)

		updateAccountBalance(&account, entry.Credit, entry.Debit)
		s.accounts[account.ID] = account
	}

	transaction := &Transaction{
		ID:        transactionID,
		Entries:   transactionEntries,
		Timestamp: sec,
	}

	s.transactions = append(s.transactions, *transaction)
	return transaction, nil
}

func (s *MemoryStore) getTransactionByID(transactionID string) (*Transaction, error) {
	for _, transaction := range s.transactions {
		if transaction.ID == transactionID {
			return &transaction, nil
		}
	}
	return nil, fmt.Errorf("Transaction with ID %s not found", transactionID)
}

func updateAccountBalance(account *Account, credit decimal.Decimal, debit decimal.Decimal) {
	account.Balance = account.Balance.Add(debit).Sub(credit)
}