package ledger

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestCreateAccount(t *testing.T) {
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
		{AssetType, 0, 100, -100},    // credit-normal: -100
		{ExpenseType, 100, 0, 100},   // debit-normal: +100
		{LiabilityType, 0, 100, 100}, // credit-normal: stored -100, returned +100
		{EquityType, 0, 100, 100},    // credit-normal: stored -100, returned +100
		{RevenueType, 0, 100, 100},   // credit-normal: stored -100, returned +100
	}

	s := NewMemoryStore()
	for _, tc := range cases {
		subject, err1 := s.CreateAccount("subject", tc.accType)
		other, err2 := s.CreateAccount("other", AssetType)

		if err1 != nil {
			t.Fatalf("Unexpected error creating subject account: %v", err1)
		}
		if err2 != nil {
			t.Fatalf("Unexpected error creating other account: %v", err2)
		}

		_, _, err := s.CreateTransaction("", []EntryRequest{
			{AccountID: subject.ID, Debit: decimal.NewFromInt(tc.debit), Credit: decimal.NewFromInt(tc.credit)},
			{AccountID: other.ID, Debit: decimal.NewFromInt(tc.credit), Credit: decimal.NewFromInt(tc.debit)},
		})
		if err != nil {
			t.Fatalf("Unexpected error creating transaction: %v", err)
		}

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
	_, _, err = s.CreateTransaction("", entries)
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
	_, _, err = s.CreateTransaction("", entries)
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

func TestCreateTransaction_Idempotency(t *testing.T) {
	cases := []struct {
		name       string
		key1       string
		key2       string
		sameID     bool
		existedOn2 bool
	}{
		{"same key", "key-abc", "key-abc", true, true},
		{"different keys", "key-1", "key-2", false, false},
		{"empty key", "", "", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMemoryStore()
			acc1, _ := s.CreateAccount("Cash", AssetType)
			acc2, _ := s.CreateAccount("Revenue", RevenueType)
			entries := []EntryRequest{
				{AccountID: acc1.ID, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
				{AccountID: acc2.ID, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
			}

			tx1, existed1, err := s.CreateTransaction(tc.key1, entries)
			if err != nil || existed1 {
				t.Fatalf("first call: expected new transaction, got existed=%v err=%v", existed1, err)
			}

			tx2, existed2, err := s.CreateTransaction(tc.key2, entries)
			if err != nil {
				t.Fatalf("second call: unexpected error: %v", err)
			}
			if existed2 != tc.existedOn2 {
				t.Errorf("existed: expected %v, got %v", tc.existedOn2, existed2)
			}
			if tc.sameID && tx1.ID != tx2.ID {
				t.Errorf("expected same transaction ID, got %s and %s", tx1.ID, tx2.ID)
			}
			if !tc.sameID && tx1.ID == tx2.ID {
				t.Errorf("expected different transaction IDs, got same: %s", tx1.ID)
			}
		})
	}
}
