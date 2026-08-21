package ledger

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func setupTransfer(t *testing.T) (*MemoryStore, TransferRequest) {
	t.Helper()
	s := NewMemoryStore()
	source, err := s.CreateAccountInLedger("ledger-source", "Cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	sourceOffset, err := s.CreateAccountInLedger("ledger-source", "Opening balance", RevenueType)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := s.CreateAccountInLedger("ledger-destination", "Cash", AssetType)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTransaction("fund-source", []EntryRequest{{AccountID: source.ID, Debit: decimal.NewFromInt(100)}, {AccountID: sourceOffset.ID, Credit: decimal.NewFromInt(100)}}); err != nil {
		t.Fatal(err)
	}
	return s, TransferRequest{SourceLedgerID: source.LedgerID, SourceAccountID: source.ID, DestinationLedgerID: destination.LedgerID, DestinationAccountID: destination.ID, Amount: decimal.NewFromInt(10), IdempotencyKey: "transfer-1"}
}

func TestCrossLedgerTransferCompletes(t *testing.T) {
	s, req := setupTransfer(t)
	transfer, existed, err := s.CreateTransfer(req)
	if err != nil || existed || transfer.Status != TransferPending {
		t.Fatalf("create transfer: existed=%v status=%s err=%v", existed, transfer.Status, err)
	}
	if n, err := s.ProcessOutbox(10); err != nil || n != 1 {
		t.Fatalf("process: n=%d err=%v", n, err)
	}
	got, _ := s.GetTransfer(transfer.ID)
	if got.Status != TransferCompleted {
		t.Fatalf("status = %s", got.Status)
	}
	if balance, _ := s.GetBalance(req.SourceAccountID); !balance.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("source balance = %s", balance)
	}
	if balance, _ := s.GetBalance(req.DestinationAccountID); !balance.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("destination balance = %s", balance)
	}
}

func TestTransferRecoversAfterCrashFollowingSourceDebit(t *testing.T) {
	s, req := setupTransfer(t)
	transfer, _, err := s.CreateTransfer(req)
	if err != nil {
		t.Fatal(err)
	}
	// A process restart sees the durable outbox event before any destination
	// posting. Calling the worker later is equivalent to its startup scan.
	if balance, _ := s.GetBalance(req.SourceAccountID); !balance.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("source debit was not recorded: %s", balance)
	}
	if _, err := s.ProcessOutbox(10); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTransfer(transfer.ID)
	if got.Status != TransferCompleted {
		t.Fatalf("recovered status = %s", got.Status)
	}
}

func TestDestinationFailureCompensatesSource(t *testing.T) {
	s, req := setupTransfer(t)
	s.DestinationFailure = func(TransferRequest) error { return errors.New("destination ledger unavailable permanently") }
	transfer, _, err := s.CreateTransfer(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessOutbox(10); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTransfer(transfer.ID)
	if got.Status != TransferCompensated {
		t.Fatalf("status = %s", got.Status)
	}
	if balance, _ := s.GetBalance(req.SourceAccountID); !balance.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("source was not compensated: %s", balance)
	}
	if balance, _ := s.GetBalance(req.DestinationAccountID); !balance.IsZero() {
		t.Fatalf("destination was credited: %s", balance)
	}
}

func TestDuplicateTransferAndEventDoNotDuplicateCredit(t *testing.T) {
	s, req := setupTransfer(t)
	first, existed, err := s.CreateTransfer(req)
	if err != nil || existed {
		t.Fatalf("first create: existed=%v err=%v", existed, err)
	}
	second, existed, err := s.CreateTransfer(req)
	if err != nil || !existed || second.ID != first.ID {
		t.Fatalf("duplicate create: existed=%v same=%v err=%v", existed, second != nil && second.ID == first.ID, err)
	}
	if _, err := s.ProcessOutbox(10); err != nil {
		t.Fatal(err)
	}
	// A second polling pass simulates duplicate delivery after the first worker
	// has committed. The processed marker prevents another destination credit.
	if _, err := s.ProcessOutbox(10); err != nil {
		t.Fatal(err)
	}
	if balance, _ := s.GetBalance(req.DestinationAccountID); !balance.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("duplicate credit: %s", balance)
	}
}
