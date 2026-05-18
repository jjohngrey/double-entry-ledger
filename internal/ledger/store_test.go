package ledger

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestCreateAccount_ValidInput(t *testing.T) {
	s := NewMemoryStore()

	var account, err = s.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Errorf("Unexpected error for valid account creation: %v", err)
	}
	if account == nil {
		t.Errorf("Expected non-nil account for valid input, got nil")
	} else {
		if account.Name != "Cash" {
			t.Errorf("Expected account name 'Cash', got '%s'", account.Name)
		}
		if account.Type != AssetType {
			t.Errorf("Expected account type 'asset', got '%s'", account.Type)
		}
		if !account.Balance.Equal(decimal.Zero) {
			t.Errorf("Expected initial balance of 0, got %s", account.Balance)
		}
	}
}

func TestCreateAccount_InvalidInput(t *testing.T) {
	s := NewMemoryStore()

	cases := []struct {
		name    string
		accName string
		accType AccountType
	}{
		{"empty name", "", AssetType},
		{"invalid type", "Cash", "invalid_type"},
	}

	for _, tc := range cases {
		acc, err := s.CreateAccount(tc.accName, tc.accType)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
		if acc != nil {
			t.Errorf("%s: expected nil account, got %+v", tc.name, acc)
		}
	}
}

func TestGetBalance_NonExistentAccount(t *testing.T) {
	s := NewMemoryStore()

	_, err := s.GetBalance("non-existent-id")
	if err == nil {
		t.Errorf("Expected error for non-existent account, got nil")
	}
}

func TestGetBalance_ExistingAccount(t *testing.T) {
	s := NewMemoryStore()

	account, err := s.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatalf("Unexpected error creating account: %v", err)
	}

	balance, err := s.GetBalance(account.ID)
	if err != nil {
		t.Errorf("Unexpected error getting balance: %v", err)
	}
	if !balance.Equal(decimal.Zero) {
		t.Errorf("Expected initial balance of 0, got %s", balance)
	}
}

func TestGetBalance_SignFlipByAccountType(t *testing.T) {
	cases := []struct {
		accType AccountType
		debit   int64
		credit  int64
		wantBal int64
	}{
		{AssetType, 100, 0, 100},     // debit-normal: +100
		{ExpenseType, 100, 0, 100},   // debit-normal: +100
		{LiabilityType, 0, 100, 100}, // credit-normal: stored -100, returned +100
		{EquityType, 0, 100, 100},    // credit-normal: stored -100, returned +100
		{RevenueType, 0, 100, 100},   // credit-normal: stored -100, returned +100
	}

	for _, tc := range cases {
		s := NewMemoryStore()
		subject, _ := s.CreateAccount("subject", tc.accType)
		other, _ := s.CreateAccount("other", AssetType)

		s.CreateTransaction([]EntryRequest{
			{AccountID: subject.ID, Debit: decimal.NewFromInt(tc.debit), Credit: decimal.NewFromInt(tc.credit)},
			{AccountID: other.ID, Debit: decimal.NewFromInt(tc.credit), Credit: decimal.NewFromInt(tc.debit)},
		})

		got, _ := s.GetBalance(subject.ID)
		want := decimal.NewFromInt(tc.wantBal)
		if !got.Equal(want) {
			t.Errorf("%s: got %s, want %s", tc.accType, got, want)
		}
	}
}

func TestCreateTransaction_Balanced(t *testing.T) {
	s := NewMemoryStore()

	// Create two accounts
	acc1, err := s.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatalf("Unexpected error creating account 1: %v", err)
	}
	acc2, err := s.CreateAccount("Revenue", RevenueType)
	if err != nil {
		t.Fatalf("Unexpected error creating account 2: %v", err)
	}

	// Create a balanced transaction: Debit Cash $100, Credit Revenue $100
	entries := []EntryRequest{
		{AccountID: acc1.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		{AccountID: acc2.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
	}
	_, err = s.CreateTransaction(entries)
	if err != nil {
		t.Fatalf("Unexpected error creating transaction: %v", err)
	}

	// Check balances
	balance1, err := s.GetBalance(acc1.ID)
	if err != nil {
		t.Errorf("Unexpected error getting balance for account 1: %v", err)
	}
	if !balance1.Equal(decimal.NewFromInt(100)) {
		t.Errorf("Expected balance of 100 for account 1, got %s", balance1)
	}

	balance2, err := s.GetBalance(acc2.ID)
	if err != nil {
		t.Errorf("Unexpected error getting balance for account 2: %v", err)
	}
	if !balance2.Equal(decimal.NewFromInt(100)) {
		t.Errorf("Expected balance of 100 for account 2, got %s", balance2)
	}
}

func TestCreateTransaction_Unbalanced(t *testing.T) {
	s := NewMemoryStore()

	// Create two accounts
	acc1, err := s.CreateAccount("Cash", AssetType)
	if err != nil {
		t.Fatalf("Unexpected error creating account 1: %v", err)
	}
	acc2, err := s.CreateAccount("Revenue", RevenueType)
	if err != nil {
		t.Fatalf("Unexpected error creating account 2: %v", err)
	}

	// Create an unbalanced transaction: Debit Cash $100, Credit Revenue $50
	entries := []EntryRequest{
		{AccountID: acc1.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		{AccountID: acc2.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(50)},
	}
	_, err = s.CreateTransaction(entries)
	if err == nil {
		t.Fatal("Expected error for unbalanced transaction, got nil")
	}

	// Check that balances are unchanged
	balance1, err := s.GetBalance(acc1.ID)
	if err != nil {
		t.Errorf("Unexpected error getting balance for account 1: %v", err)
	}
	if !balance1.Equal(decimal.Zero) {
		t.Errorf("Expected balance of 0 for account 1, got %s", balance1)
	}
	balance2, err := s.GetBalance(acc2.ID)
	if err != nil {
		t.Errorf("Unexpected error getting balance for account 2: %v", err)
	}
	if !balance2.Equal(decimal.Zero) {
		t.Errorf("Expected balance of 0 for account 2, got %s", balance2)
	}
}

func TestCreateTransaction_InvalidInput(t *testing.T) {
	s := NewMemoryStore()
	
	var acc, err = s.CreateAccount("existent", AssetType)
	if err != nil {
		t.Fatalf("Unexpected error creating account: %v", err)
	}
	cases := []struct {
		name    string
		entries []EntryRequest
	}{
		{"less than 2 entries", []EntryRequest{{AccountID: "some-id", Debit: decimal.NewFromInt(100), Credit: decimal.Zero}}},
		{"account does not exist", []EntryRequest{
			{AccountID: acc.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: "other-non-existent-id", Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
		}},
		{"negative debit", []EntryRequest{
			{AccountID: "some-id", Debit: decimal.NewFromInt(-100), Credit: decimal.Zero},
			{AccountID: "other-id", Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
		}},
		{"negative credit", []EntryRequest{
			{AccountID: "some-id", Debit: decimal.Zero, Credit: decimal.NewFromInt(-100)},
			{AccountID: "other-id", Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		}},
		{"debits do not equal credits", []EntryRequest{
			{AccountID: "some-id", Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: "other-id", Debit: decimal.Zero, Credit: decimal.NewFromInt(50)},
		}},
	}


	for _, tc := range cases {
		_, err := s.CreateTransaction(tc.entries)
		if err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}
