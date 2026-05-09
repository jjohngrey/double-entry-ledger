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

func ValidateTransaction(txn *Transaction) error {
	if len(txn.Entries) < 2 {
		return fmt.Errorf("Transaction must have at least two entries")
	}

	var totalDebit, totalCredit decimal.Decimal
	for _, entry := range txn.Entries {
		if entry.AccountID == "" {
			return fmt.Errorf("Entry account ID cannot be empty")
		}
		if entry.Debit.IsNegative() || entry.Credit.IsNegative() {
			return fmt.Errorf("Entry amounts cannot be negative")
		}
		totalDebit = totalDebit.Add(entry.Debit)
		totalCredit = totalCredit.Add(entry.Credit)
	}
	if !totalDebit.Equal(totalCredit) {
		return fmt.Errorf("Total debits (%s) must equal total credits (%s)", totalDebit, totalCredit)
	}
	return nil
}