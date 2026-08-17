package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const transactionAmountScale int32 = 2

// IdempotencyConflictError reports reuse of an idempotency key for a different
// transaction request.
type IdempotencyConflictError struct{}

func (IdempotencyConflictError) Error() string {
	return "idempotency key was already used with a different request"
}

// ErrIdempotencyKeyConflict allows callers to map an idempotency conflict to
// an HTTP 409 response with errors.Is.
var ErrIdempotencyKeyConflict error = IdempotencyConflictError{}

type canonicalTransactionEntry struct {
	AccountID string `json:"account_id"`
	Credit    string `json:"credit"`
	Debit     string `json:"debit"`
}

type canonicalTransactionRequest struct {
	LedgerID string                      `json:"ledger_id"`
	Entries  []canonicalTransactionEntry `json:"entries"`
}

// TransactionRequestChecksum returns the SHA-256 checksum of a canonical
// transaction request. Amounts are normalized to the database's fixed
// two-decimal scale, and entries are sorted so their input order is irrelevant.
func TransactionRequestChecksum(ledgerID string, entries []EntryRequest) string {
	canonicalEntries := make([]canonicalTransactionEntry, len(entries))
	for i, entry := range entries {
		canonicalEntries[i] = canonicalTransactionEntry{
			AccountID: strings.ToLower(entry.AccountID),
			Credit:    entry.Credit.StringFixed(transactionAmountScale),
			Debit:     entry.Debit.StringFixed(transactionAmountScale),
		}
	}
	sort.Slice(canonicalEntries, func(i, j int) bool {
		left, right := canonicalEntries[i], canonicalEntries[j]
		if left.AccountID != right.AccountID {
			return left.AccountID < right.AccountID
		}
		if left.Debit != right.Debit {
			return left.Debit < right.Debit
		}
		return left.Credit < right.Credit
	})

	canonicalBytes, _ := json.Marshal(canonicalTransactionRequest{
		LedgerID: ledgerID,
		Entries:  canonicalEntries,
	})
	sum := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(sum[:])
}
