package ledger

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type Store struct {
	mu           sync.RWMutex       // single lock protects all three below
	accounts     map[string]Account // UUID -> Account
	transactions []Transaction
	entries      []Entry
}

// Constructor
func NewStore() *Store {
	return &Store{
		accounts:     make(map[string]Account),
		transactions: []Transaction{},
		entries:      []Entry{},
	}
}

// Account operations
func (s *Store) CreateAccount(name string, accType AccountType) (*Account, error) {
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

func (s *Store) GetBalance(accountID string) (decimal.Decimal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[accountID]
	if !exists {
		return decimal.Zero, fmt.Errorf("Account with ID %s not found", accountID)
	}
	return account.Balance, nil
}

func (s *Store) CreateTransaction(entries []EntryRequest) (*Transaction, error) {
	if err := ValidateTransaction(entries); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id := uuid.New().String()
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
			ID:        entryID,
			AccountID: account.ID,
			Credit:    entry.Credit,
			Debit:     entry.Debit,
		}

		s.entries = append(s.entries, *newEntry)
		transactionEntries = append(transactionEntries, *newEntry)

		// update account balance
		account.Balance = account.Balance.Add(entry.Debit).Sub(entry.Credit)
		s.accounts[account.ID] = account
	}

	transaction := &Transaction{
		ID:        id,
		Entries:   transactionEntries,
		Timestamp: sec,
	}

	s.transactions = append(s.transactions, *transaction)
	return transaction, nil
}

func (s *Store) GetAccount(accountID string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	account, exists := s.accounts[accountID]
	if !exists {
		return nil, fmt.Errorf("Account with ID %s not found", accountID)
	}
	return &account, nil
}