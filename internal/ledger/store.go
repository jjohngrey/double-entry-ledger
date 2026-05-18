package ledger

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Store interface {
	CreateAccount(name string, accType AccountType) (*Account, error)
	CreateTransaction(entries []EntryRequest) (*Transaction, error)
	GetBalance(accountID string) (decimal.Decimal, error)
}

var _ Store = (*MemoryStore)(nil) // compile-time assertion that MemoryStore implements Store

type MemoryStore struct {
	mu           sync.RWMutex
	accounts     map[string]Account // UUID -> Account
	transactions []Transaction
}

// Constructor
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts:     make(map[string]Account),
		transactions: []Transaction{},
	}
}

// Account operations
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

func (s *MemoryStore) CreateTransaction(entries []EntryRequest) (*Transaction, error) {
	if err := ValidateTransaction(entries); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	transactionID := uuid.New().String()
	sec := time.Now()

	var transactionEntries []Entry

	// validate accounts exist before changes for atomicity of transaction
	for _, entry := range entries {
		_, exists := s.accounts[entry.AccountID]
		if !exists {
			return nil, fmt.Errorf("Account with ID %s not found", entry.AccountID)
		}
	}

	for _, entry := range entries {
		account := s.accounts[entry.AccountID]

		// create entries
		entryID := uuid.New().String()
		newEntry := &Entry{
			ID:            entryID,
			AccountID:     account.ID,
			TransactionID: transactionID,
			Credit:        entry.Credit,
			Debit:         entry.Debit,
		}

		transactionEntries = append(transactionEntries, *newEntry)

		// update account balance
		account.Balance = account.Balance.Add(entry.Debit).Sub(entry.Credit)
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
