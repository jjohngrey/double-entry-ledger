package ledger

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestValidateAccount(t *testing.T) {
	cases := []struct {
		name    string
		accName string
		accType AccountType
		err     bool
	}{
		{"valid asset account", "Cash", AssetType, false},
		{"empty name", "", AssetType, true},
		{"invalid type", "Cash", "invalid_type", true},
	}

	for _, tc := range cases {
		err := ValidateAccount(tc.accName, tc.accType)
		if tc.err && err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		} else if !tc.err && err != nil {
			t.Errorf("%s: expected no error, got %v", tc.name, err)
		}
	}
}

func TestValidateTransaction(t *testing.T) {
	acc1 := "acc1"
	acc2 := "acc2"

	cases := []struct {
		name    string
		entries []EntryRequest
		err     bool
	}{
		{"with zero entries", []EntryRequest{}, true},
		{"less than 2 entries", []EntryRequest{{AccountID: acc1, Debit: decimal.NewFromInt(100), Credit: decimal.Zero}}, true},
		{"negative debit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.NewFromInt(-100), Credit: decimal.Zero},
			{AccountID: acc2, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
		}, true},
		{"negative credit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.Zero, Credit: decimal.NewFromInt(-100)},
			{AccountID: acc2, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		}, true},
		{"entry with zero debit and non-zero credit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.Zero, Credit: decimal.Zero},
			{AccountID: acc2, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		}, true},
		{"entry with non-zero debit and zero credit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: acc2, Debit: decimal.Zero, Credit: decimal.Zero},
		}, true},
		{"entry with both debit and credit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.NewFromInt(50), Credit: decimal.NewFromInt(50)},
			{AccountID: acc2, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		}, true},
		{"valid debit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: acc2, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
		}, false},
		{"valid credit", []EntryRequest{
			{AccountID: acc1, Debit: decimal.Zero, Credit: decimal.NewFromInt(100)},
			{AccountID: acc2, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
		}, false},
		{"debits do not equal credits", []EntryRequest{
			{AccountID: acc1, Debit: decimal.NewFromInt(100), Credit: decimal.Zero},
			{AccountID: acc2, Debit: decimal.Zero, Credit: decimal.NewFromInt(50)},
		}, true},
	}

	for _, tc := range cases {
		err := ValidateTransaction(tc.entries)
		if tc.err && err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		} else if !tc.err && err != nil {
			t.Errorf("%s: expected no error, got %v", tc.name, err)
		}
	}
}
