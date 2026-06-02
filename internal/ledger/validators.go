package ledger

import (
	"fmt"

	"github.com/shopspring/decimal"
)

func ValidateAccount(name string, typ AccountType) error {
	if name == "" {
		return fmt.Errorf("Account name cannot be empty")
	}
	switch typ {
	case AssetType, LiabilityType, EquityType, RevenueType, ExpenseType:
		return nil
	default:
		return fmt.Errorf("Invalid account type: %s", typ)
	}
}

func ValidateTransaction(entries []EntryRequest) error {
	if len(entries) < 2 {
		return fmt.Errorf("Transaction must have at least two entries")
	}

	var totalDebit, totalCredit decimal.Decimal
	for _, entry := range entries {
		if entry.Debit.IsNegative() || entry.Credit.IsNegative() {
			return fmt.Errorf("Entry amounts cannot be negative")
		}
		if entry.Debit.IsZero() && entry.Credit.IsZero() {
			return fmt.Errorf("Entry must have a non-zero debit or credit amount")
		}
		if !entry.Debit.IsZero() && !entry.Credit.IsZero() {
			return fmt.Errorf("Entry cannot have both debit and credit amounts")
		}
		totalDebit = totalDebit.Add(entry.Debit)
		totalCredit = totalCredit.Add(entry.Credit)
	}
	if !totalDebit.Equal(totalCredit) {
		return fmt.Errorf("Total debits (%s) must equal total credits (%s)", totalDebit, totalCredit)
	}
	return nil
}