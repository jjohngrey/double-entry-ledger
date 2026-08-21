package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var _ TransferStore = (*PostgresStore)(nil)

func (s *PostgresStore) CreateAccountInLedger(ledgerID, name string, typ AccountType) (*Account, error) {
	if err := ValidateAccount(name, typ); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(ledgerID)
	if err != nil {
		return nil, fmt.Errorf("invalid ledger ID: %w", err)
	}
	var accountID uuid.UUID
	err = s.db.QueryRow(`INSERT INTO accounts (ledger_id, name, type) VALUES ($1, $2, $3) RETURNING id`, id, name, typ).Scan(&accountID)
	if err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	return &Account{ID: accountID.String(), LedgerID: ledgerID, Name: name, Type: typ, Balance: decimal.Zero}, nil
}

func (s *PostgresStore) CreateTransfer(req TransferRequest) (*TransferResponse, bool, error) {
	if err := validateTransfer(req); err != nil {
		return nil, false, err
	}
	sourceLedger, err := uuid.Parse(req.SourceLedgerID)
	if err != nil {
		return nil, false, fmt.Errorf("invalid source ledger ID: %w", err)
	}
	destinationLedger, err := uuid.Parse(req.DestinationLedgerID)
	if err != nil {
		return nil, false, fmt.Errorf("invalid destination ledger ID: %w", err)
	}
	source, err := uuid.Parse(req.SourceAccountID)
	if err != nil {
		return nil, false, fmt.Errorf("invalid source account ID: %w", err)
	}
	destination, err := uuid.Parse(req.DestinationAccountID)
	if err != nil {
		return nil, false, fmt.Errorf("invalid destination account ID: %w", err)
	}
	if sourceLedger == destinationLedger {
		return nil, false, errors.New("transfer requires distinct source and destination ledgers")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, s.txOptions)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if existing, err := scanSaga(ctx, tx, req.IdempotencyKey); err == nil {
		if !sameTransfer(*existing, req) {
			return nil, false, ErrTransferIdempotencyConflict
		}
		return existing, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var actualSourceLedger, actualDestinationLedger uuid.UUID
	var sourceType, sourceBalanceText string
	err = tx.QueryRowContext(ctx, `
		SELECT source.ledger_id,source.type::text,source.balance::text,destination.ledger_id
		FROM accounts source
		JOIN accounts destination ON destination.id=$2
		WHERE source.id=$1
		FOR UPDATE OF source`, source, destination).Scan(&actualSourceLedger, &sourceType, &sourceBalanceText, &actualDestinationLedger)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, errors.New("source or destination account not found")
	}
	if err != nil {
		return nil, false, err
	}
	if actualSourceLedger != sourceLedger {
		return nil, false, errors.New("source: account does not belong to ledger")
	}
	if actualDestinationLedger != destinationLedger {
		return nil, false, errors.New("destination: account does not belong to ledger")
	}
	sourceBalance, err := decimal.NewFromString(sourceBalanceText)
	if err != nil {
		return nil, false, fmt.Errorf("parse source balance: %w", err)
	}
	if displayedBalanceAfter(AccountType(sourceType), sourceBalance, EntryRequest{Credit: req.Amount}).IsNegative() {
		return nil, false, fmt.Errorf("insufficient funds for account %s", source)
	}
	clearing, err := ensureClearing(ctx, tx, sourceLedger)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	id := uuid.New()
	if err := writeSourceTransfer(ctx, tx, id, req.IdempotencyKey, sourceLedger, source, clearing, destinationLedger, destination, req.Amount, now); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &TransferResponse{ID: id.String(), SourceLedgerID: req.SourceLedgerID, SourceAccountID: req.SourceAccountID, DestinationLedgerID: req.DestinationLedgerID, DestinationAccountID: req.DestinationAccountID, Amount: req.Amount, Status: TransferPending, CreatedAt: now, UpdatedAt: now}, false, nil
}

func writeSourceTransfer(ctx context.Context, tx *sql.Tx, sagaID uuid.UUID, idempotencyKey string, sourceLedger, source, clearing, destinationLedger, destination uuid.UUID, amount decimal.Decimal, now time.Time) error {
	transactionID, sourceEntryID, clearingEntryID := uuid.New(), uuid.New(), uuid.New()
	_, err := tx.ExecContext(ctx, `
		WITH inserted_transaction AS (
			INSERT INTO transactions (id,ledger_id) VALUES ($1,$2) RETURNING id
		), inserted_entries AS (
			INSERT INTO entries (id,account_id,transaction_id,credit,debit)
			SELECT value.id,value.account_id,inserted_transaction.id,value.credit,value.debit
			FROM (VALUES ($3::uuid,$4::uuid,$5::numeric,0::numeric),($6::uuid,$7::uuid,0::numeric,$5::numeric)) AS value(id,account_id,credit,debit)
			CROSS JOIN inserted_transaction RETURNING id
		), updated_accounts AS (
			UPDATE accounts SET balance=balance+CASE WHEN id=$4 THEN -$5::numeric ELSE $5::numeric END
			WHERE id IN ($4,$7) RETURNING id
		), inserted_saga AS (
			INSERT INTO sagas (id,idempotency_key,source_ledger_id,source_account_id,destination_ledger_id,destination_account_id,amount,status,created_at,updated_at)
			VALUES ($8,$9,$2,$4,$10,$11,$5,'pending',$12,$12) RETURNING id
		), inserted_steps AS (
			INSERT INTO saga_steps (saga_id,step,status,attempt_count)
			SELECT id,'source_debit','completed',1 FROM inserted_saga
			UNION ALL SELECT id,'destination_credit','pending',0 FROM inserted_saga RETURNING saga_id
		), transaction_event AS (
			INSERT INTO outbox_events (transaction_id,event_type,status,available_at)
			SELECT id,'transaction_posted','pending',NOW() FROM inserted_transaction RETURNING id
		)
		INSERT INTO outbox_events (saga_id,event_type,status,available_at)
		SELECT id,'destination_credit','pending',NOW() FROM inserted_saga`,
		transactionID, sourceLedger, sourceEntryID, source, amount.String(), clearingEntryID, clearing,
		sagaID, idempotencyKey, destinationLedger, destination, now)
	if err != nil {
		return fmt.Errorf("write source transfer: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetTransfer(id string) (*TransferResponse, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	return scanSaga(context.Background(), s.db, parsed.String())
}

func (s *PostgresStore) ProcessOutbox(limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	completed := 0
	for completed < limit {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, backgroundTxOptions)
		if err != nil {
			return completed, err
		}
		var eventID, sagaID uuid.UUID
		var typ string
		var attempts int
		err = tx.QueryRowContext(ctx, `SELECT id, saga_id, event_type, attempt_count FROM outbox_events WHERE status = 'pending' AND event_type IN ('destination_credit', 'compensate_source') AND available_at <= NOW() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&eventID, &sagaID, &typ, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			break
		}
		if err != nil {
			tx.Rollback()
			return completed, err
		}
		response, err := scanSaga(ctx, tx, sagaID.String())
		if err == nil {
			sourceLedger, _ := uuid.Parse(response.SourceLedgerID)
			destinationLedger, _ := uuid.Parse(response.DestinationLedgerID)
			source, _ := uuid.Parse(response.SourceAccountID)
			destination, _ := uuid.Parse(response.DestinationAccountID)
			switch typ {
			case "destination_credit":
				// A failed posting can put PostgreSQL in an aborted transaction.
				// Keep it behind a savepoint so we can durably schedule the
				// compensating credit in the same outbox transaction.
				if _, err = tx.ExecContext(ctx, `SAVEPOINT destination_post`); err != nil {
					break
				}
				clearing, e := ensureClearing(ctx, tx, destinationLedger)
				if e == nil {
					_, e = postLeg(ctx, tx, destinationLedger, destination, clearing, response.Amount, false)
				}
				if e != nil {
					if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT destination_post`); rollbackErr != nil {
						err = rollbackErr
						break
					}
					err = e
				} else {
					if _, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT destination_post`); err != nil {
						break
					}
					_, err = tx.ExecContext(ctx, `UPDATE sagas SET status='completed', updated_at=NOW() WHERE id=$1`, sagaID)
					if err == nil {
						_, err = tx.ExecContext(ctx, `UPDATE saga_steps SET status='completed', attempt_count=attempt_count+1 WHERE saga_id=$1 AND step='destination_credit'`, sagaID)
					}
				}
			case "compensate_source":
				clearing, e := ensureClearing(ctx, tx, sourceLedger)
				if e == nil {
					_, e = postLeg(ctx, tx, sourceLedger, source, clearing, response.Amount, false)
				}
				if e != nil {
					err = e
				} else {
					_, err = tx.ExecContext(ctx, `UPDATE sagas SET status='compensated', updated_at=NOW() WHERE id=$1`, sagaID)
				}
			default:
				err = fmt.Errorf("unknown outbox event %q", typ)
			}
		}
		if err != nil && isTransientOutboxError(err) && attempts < 5 {
			backoff := time.Duration(1<<uint(attempts)) * 100 * time.Millisecond
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET attempt_count=attempt_count+1, available_at=NOW()+$2::interval WHERE id=$1`, eventID, backoff.String())
			if err != nil {
				tx.Rollback()
				return completed, err
			}
			if err = tx.Commit(); err != nil {
				return completed, err
			}
			completed++
			continue
		}
		if err != nil { // permanent failure: retain the original debit and schedule an immutable compensation.
			_, err = tx.ExecContext(ctx, `UPDATE sagas SET status='failed', updated_at=NOW() WHERE id=$1`, sagaID)
			if err == nil && typ == "destination_credit" {
				_, err = tx.ExecContext(ctx, `UPDATE saga_steps SET status='failed', attempt_count=attempt_count+1, error='destination posting failed', updated_at=NOW() WHERE saga_id=$1 AND step='destination_credit'`, sagaID)
			}
			if err == nil && typ == "destination_credit" {
				_, err = tx.ExecContext(ctx, `INSERT INTO outbox_events (saga_id,event_type,status,available_at) VALUES ($1,'compensate_source','pending',NOW()) ON CONFLICT (saga_id,event_type) DO NOTHING`, sagaID)
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE outbox_events SET status='processed', processed_at=NOW(), attempt_count=attempt_count+1 WHERE id=$1`, eventID)
		}
		if err != nil {
			tx.Rollback()
			return completed, err
		}
		if err = tx.Commit(); err != nil {
			return completed, err
		}
		s.transferWaiters.notify(sagaID.String())
		completed++
	}
	return completed, nil
}

func (s *PostgresStore) WaitForTransfer(ctx context.Context, id string, timeout time.Duration) (*TransferResponse, error) {
	return waitForCompletion(ctx, timeout, func() (*TransferResponse, bool, error) {
		transfer, err := s.GetTransfer(id)
		if err != nil {
			return transfer, false, err
		}
		complete := transfer.Status == TransferCompleted || transfer.Status == TransferFailed || transfer.Status == TransferCompensated
		return transfer, complete, nil
	}, s.transferWaiters, id)
}

func isTransientOutboxError(err error) bool {
	if isSerializationFailure(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var transient TransientError
	return errors.As(err, &transient) && transient.Transient()
}

type sagaScanner interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func scanSaga(ctx context.Context, q sagaScanner, key string) (*TransferResponse, error) {
	var r TransferResponse
	var amount string
	query := `SELECT id,source_ledger_id,source_account_id,destination_ledger_id,destination_account_id,amount,status,created_at,updated_at FROM sagas WHERE idempotency_key=$1`
	argument := any(key)
	if id, parseErr := uuid.Parse(key); parseErr == nil {
		query = `SELECT id,source_ledger_id,source_account_id,destination_ledger_id,destination_account_id,amount,status,created_at,updated_at FROM sagas WHERE id=$1`
		argument = id
	}
	err := q.QueryRowContext(ctx, query, argument).Scan(&r.ID, &r.SourceLedgerID, &r.SourceAccountID, &r.DestinationLedgerID, &r.DestinationAccountID, &amount, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.Amount, err = decimal.NewFromString(amount)
	return &r, err
}
func verifyAccountBoundary(ctx context.Context, tx *sql.Tx, account, ledger uuid.UUID) error {
	var found uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT ledger_id FROM accounts WHERE id=$1`, account).Scan(&found)
	if err != nil {
		return err
	}
	if found != ledger {
		return errors.New("account does not belong to ledger")
	}
	return nil
}
func ensureClearing(ctx context.Context, tx *sql.Tx, ledger uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE ledger_id=$1 AND name='__transfer_clearing__'`, ledger).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `INSERT INTO accounts (ledger_id,name,type) VALUES ($1,'__transfer_clearing__','asset') ON CONFLICT (ledger_id,name) DO UPDATE SET name=EXCLUDED.name RETURNING id`, ledger).Scan(&id)
	}
	return id, err
}
func postLeg(ctx context.Context, tx *sql.Tx, ledger, account, clearing uuid.UUID, amount decimal.Decimal, source bool) (uuid.UUID, error) {
	var id uuid.UUID
	var err error
	if err := tx.QueryRowContext(ctx, `INSERT INTO transactions (ledger_id) VALUES ($1) RETURNING id`, ledger).Scan(&id); err != nil {
		return id, err
	}
	if source {
		_, err = tx.ExecContext(ctx, `INSERT INTO entries (account_id,transaction_id,credit,debit) VALUES ($1,$3,$4,0),($2,$3,0,$4)`, account, clearing, id, amount.String())
		if err != nil {
			return id, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET balance=balance+CASE WHEN id=$1 THEN -$3::numeric ELSE $3::numeric END WHERE id IN ($1,$2)`, account, clearing, amount.String())
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO entries (account_id,transaction_id,credit,debit) VALUES ($1,$3,0,$4),($2,$3,$4,0)`, account, clearing, id, amount.String())
		if err != nil {
			return id, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE accounts SET balance=balance+CASE WHEN id=$1 THEN $3::numeric ELSE -$3::numeric END WHERE id IN ($1,$2)`, account, clearing, amount.String())
	}
	if err != nil {
		return id, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox_events (transaction_id, event_type, status, available_at) VALUES ($1, 'transaction_posted', 'pending', NOW())`, id)
	return id, err
}
