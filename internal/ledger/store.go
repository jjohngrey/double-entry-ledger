package ledger

import (
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
