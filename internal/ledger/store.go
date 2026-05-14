package ledger

import (
	"fmt"
	"sync"

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
