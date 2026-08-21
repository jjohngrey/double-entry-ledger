package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var ErrTransferIdempotencyConflict = errors.New("idempotency key was already used for a different transfer")

// TransientError marks an error as safe to retry. All other destination
// failures are considered permanent and result in a compensating posting.
type TransientError interface{ Transient() bool }

type memorySaga struct {
	response                  TransferResponse
	sourceTransactionID       string
	destinationTransactionID  string
	compensationTransactionID string
}

type memoryOutboxEvent struct {
	id          string
	sagaID      string
	step        string
	attempts    int
	nextAttempt time.Time
	processed   bool
}

var _ TransferStore = (*MemoryStore)(nil)

func (s *MemoryStore) CreateTransfer(req TransferRequest) (*TransferResponse, bool, error) {
	if err := validateTransfer(req); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.transferKeys[req.IdempotencyKey]; ok {
		saga := s.sagas[id]
		if !sameTransfer(saga.response, req) {
			return nil, false, ErrTransferIdempotencyConflict
		}
		copy := saga.response
		return &copy, true, nil
	}
	source, ok := s.accounts[req.SourceAccountID]
	if !ok || source.LedgerID != req.SourceLedgerID {
		return nil, false, errors.New("source account does not belong to source ledger")
	}
	destination, ok := s.accounts[req.DestinationAccountID]
	if !ok || destination.LedgerID != req.DestinationLedgerID {
		return nil, false, errors.New("destination account does not belong to destination ledger")
	}
	if req.SourceLedgerID == req.DestinationLedgerID {
		return nil, false, errors.New("transfer requires distinct source and destination ledgers")
	}
	if displayedBalanceAfter(source.Type, source.Balance, EntryRequest{Credit: req.Amount}).IsNegative() {
		return nil, false, errors.New("insufficient funds")
	}

	clearing, err := s.clearingAccountLocked(req.SourceLedgerID)
	if err != nil {
		return nil, false, err
	}
	debit, err := s.createTransaction([]EntryRequest{{AccountID: source.ID, Credit: req.Amount}, {AccountID: clearing.ID, Debit: req.Amount}})
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	response := TransferResponse{ID: uuid.NewString(), SourceLedgerID: req.SourceLedgerID, SourceAccountID: req.SourceAccountID, DestinationLedgerID: req.DestinationLedgerID, DestinationAccountID: req.DestinationAccountID, Amount: req.Amount, Status: TransferPending, CreatedAt: now, UpdatedAt: now}
	s.sagas[response.ID] = &memorySaga{response: response, sourceTransactionID: debit.ID}
	s.transferKeys[req.IdempotencyKey] = response.ID
	s.outbox = append(s.outbox, &memoryOutboxEvent{id: uuid.NewString(), sagaID: response.ID, step: "destination_credit", nextAttempt: now})
	return &response, false, nil
}

func (s *MemoryStore) GetTransfer(id string) (*TransferResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	saga, ok := s.sagas[id]
	if !ok {
		return nil, fmt.Errorf("transfer %s not found", id)
	}
	copy := saga.response
	return &copy, nil
}

// ProcessOutbox is intentionally explicit: applications run it in a worker
// loop. Events are only marked processed after their journal posting and saga
// transition have both succeeded, making duplicate delivery harmless.
func (s *MemoryStore) ProcessOutbox(limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	processed := 0
	for processed < limit {
		s.mu.Lock()
		var event *memoryOutboxEvent
		now := time.Now().UTC()
		for _, candidate := range s.outbox {
			if !candidate.processed && !candidate.nextAttempt.After(now) {
				event = candidate
				break
			}
		}
		if event == nil {
			s.mu.Unlock()
			break
		}
		err := s.processEventLocked(event)
		if err != nil {
			event.attempts++
			if t, ok := err.(TransientError); ok && t.Transient() && event.attempts < 5 {
				event.nextAttempt = now.Add(time.Duration(1<<uint(event.attempts-1)) * 10 * time.Millisecond)
			} else if event.step == "destination_credit" {
				saga := s.sagas[event.sagaID]
				saga.response.Status = TransferFailed
				saga.response.UpdatedAt = now
				event.processed = true
				s.outbox = append(s.outbox, &memoryOutboxEvent{id: uuid.NewString(), sagaID: event.sagaID, step: "compensate_source", nextAttempt: now})
			} else {
				s.mu.Unlock()
				return processed, err
			}
			s.mu.Unlock()
			processed++
			continue
		}
		event.processed = true
		s.mu.Unlock()
		processed++
	}
	return processed, nil
}

func (s *MemoryStore) processEventLocked(event *memoryOutboxEvent) error {
	saga := s.sagas[event.sagaID]
	req := TransferRequest{SourceLedgerID: saga.response.SourceLedgerID, SourceAccountID: saga.response.SourceAccountID, DestinationLedgerID: saga.response.DestinationLedgerID, DestinationAccountID: saga.response.DestinationAccountID, Amount: saga.response.Amount}
	switch event.step {
	case "destination_credit":
		if saga.destinationTransactionID != "" {
			saga.response.Status = TransferCompleted
			return nil
		}
		if s.DestinationFailure != nil {
			if err := s.DestinationFailure(req); err != nil {
				return err
			}
		}
		clearing, err := s.clearingAccountLocked(req.DestinationLedgerID)
		if err != nil {
			return err
		}
		tx, err := s.createTransaction([]EntryRequest{{AccountID: req.DestinationAccountID, Debit: req.Amount}, {AccountID: clearing.ID, Credit: req.Amount}})
		if err != nil {
			return err
		}
		saga.destinationTransactionID = tx.ID
		saga.response.Status = TransferCompleted
		saga.response.UpdatedAt = time.Now().UTC()
	case "compensate_source":
		if saga.compensationTransactionID != "" {
			saga.response.Status = TransferCompensated
			return nil
		}
		clearing, err := s.clearingAccountLocked(req.SourceLedgerID)
		if err != nil {
			return err
		}
		tx, err := s.createTransaction([]EntryRequest{{AccountID: req.SourceAccountID, Debit: req.Amount}, {AccountID: clearing.ID, Credit: req.Amount}})
		if err != nil {
			return err
		}
		saga.compensationTransactionID = tx.ID
		saga.response.Status = TransferCompensated
		saga.response.UpdatedAt = time.Now().UTC()
	default:
		return fmt.Errorf("unknown outbox step %q", event.step)
	}
	return nil
}

func (s *MemoryStore) clearingAccountLocked(ledgerID string) (*Account, error) {
	if id, ok := s.clearingAccounts[ledgerID]; ok {
		a := s.accounts[id]
		return &a, nil
	}
	a := Account{ID: uuid.NewString(), LedgerID: ledgerID, Name: "__transfer_clearing__", Type: AssetType, Balance: decimal.Zero}
	s.accounts[a.ID] = a
	s.clearingAccounts[ledgerID] = a.ID
	return &a, nil
}

func validateTransfer(req TransferRequest) error {
	if req.SourceLedgerID == "" || req.SourceAccountID == "" || req.DestinationLedgerID == "" || req.DestinationAccountID == "" {
		return errors.New("source and destination ledger/account are required")
	}
	if req.IdempotencyKey == "" {
		return errors.New("idempotency key is required")
	}
	if !req.Amount.IsPositive() {
		return errors.New("amount must be positive")
	}
	return nil
}

func sameTransfer(r TransferResponse, req TransferRequest) bool {
	return r.SourceLedgerID == req.SourceLedgerID && r.SourceAccountID == req.SourceAccountID && r.DestinationLedgerID == req.DestinationLedgerID && r.DestinationAccountID == req.DestinationAccountID && r.Amount.Equal(req.Amount)
}
