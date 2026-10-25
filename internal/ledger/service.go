package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
)

var (
	ErrUnknownAccount = errors.New("one or more accounts do not exist")
	ErrMixedCurrency  = errors.New("all accounts in a transaction must share one currency")

	ErrInsufficientFunds = errors.New("insufficient funds")
)

const maxAttempts = 3

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// PostTransaction validates the request and writes the transaction and all
// its entries atomically. Posting the same idempotency key again returns the
// original transaction and writes nothing new.
//
// Deadlocks (40P01) and serialization failures (40001) are retried a few
// times; every other error is returned immediately.
func (s *Service) PostTransaction(ctx context.Context, in PostInput) (db.Transaction, error) {
	if err := validate(in); err != nil {
		return db.Transaction{}, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		t, err := s.post(ctx, in)
		if err == nil || !isRetryable(err) {
			return t, err
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return db.Transaction{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
	return db.Transaction{}, fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}

// post performs one attempt, inside a single database transaction.
func (s *Service) post(ctx context.Context, in PostInput) (db.Transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.Transaction{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // harmless after a successful Commit

	q := db.New(tx)

	// 1. Lock every account involved, in id order, before writing anything.
	//    Same order for everyone means no deadlocks.
	ids := accountIDs(in.Entries)
	accounts, err := q.LockAccountsForUpdate(ctx, ids)
	if err != nil {
		return db.Transaction{}, fmt.Errorf("lock accounts: %w", err)
	}
	if len(accounts) != len(ids) {
		return db.Transaction{}, ErrUnknownAccount
	}
	for _, a := range accounts[1:] {
		if a.Currency != accounts[0].Currency {
			return db.Transaction{}, ErrMixedCurrency
		}
	}

	// 2. Insert the transaction header. If the idempotency key already
	//    exists, ON CONFLICT DO NOTHING returns no row: this is a retry.
	t, err := q.CreateTransaction(ctx, db.CreateTransactionParams{
		IdempotencyKey: in.IdempotencyKey,
		EffectiveDate:  in.EffectiveDate,
		Description:    in.Description,
		RequestHash:    requestHash(in),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := q.GetTransactionByIdempotencyKey(ctx, in.IdempotencyKey)
		if getErr != nil {
			return db.Transaction{}, fmt.Errorf("load original transaction: %w", getErr)
		}
		return existing, nil
	}
	if err != nil {
		return db.Transaction{}, fmt.Errorf("create transaction: %w", err)
	}

	// 3. Enforce "no overdraft" on accounts that forbid it. This reads
	//    balances, which is exactly why the locks above matter: while we hold
	//    them, nobody else can change these balances under us.
	if err := checkFunds(ctx, q, accounts, in.Entries); err != nil {
		return db.Transaction{}, err
	}

	// 4. Insert the entries.
	for _, e := range in.Entries {
		_, err := q.CreateEntry(ctx, db.CreateEntryParams{
			TransactionID: t.ID,
			AccountID:     e.AccountID,
			Amount:        e.Amount,
		})
		if err != nil {
			return db.Transaction{}, fmt.Errorf("create entry: %w", err)
		}
	}

	// 5. Commit. The deferred balance trigger runs here and rejects the
	//    whole transaction if the entries do not add up.
	if err := tx.Commit(ctx); err != nil {
		return db.Transaction{}, fmt.Errorf("commit: %w", err)
	}
	return t, nil
}

// checkFunds refuses the posting if it would push an account that forbids
// overdraft onto the wrong side of zero. The accounts must already be locked.
func checkFunds(ctx context.Context, q *db.Queries, accounts []db.LockAccountsForUpdateRow, entries []EntryInput) error {
	delta := make(map[int64]int64)
	for _, e := range entries {
		delta[e.AccountID] += e.Amount
	}

	for _, a := range accounts {
		if a.AllowOverdraft {
			continue
		}
		balance, err := q.GetAccountBalance(ctx, a.ID)
		if err != nil {
			return fmt.Errorf("balance of account %d: %w", a.ID, err)
		}
		after, ok := addChecked(balance, delta[a.ID])
		if !ok {
			return ErrOverflow
		}
		if overdrawn(a.Type, after) {
			return fmt.Errorf("%w: account %d", ErrInsufficientFunds, a.ID)
		}
	}
	return nil
}

// overdrawn reports whether a signed balance (debit positive, credit negative)
// is on the wrong side of zero for the account type.
//
//	asset, expense                 normal side is debit:  balance must not go below 0
//	liability, equity, revenue     normal side is credit: balance must not go above 0
func overdrawn(accountType string, balance int64) bool {
	switch accountType {
	case "asset", "expense":
		return balance < 0
	default:
		return balance > 0
	}
}

// accountIDs returns the distinct account ids used by the entries, sorted.
func accountIDs(entries []EntryInput) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.AccountID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// isRetryable reports whether err is a deadlock or serialization failure.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40P01" || pgErr.Code == "40001"
	}
	return false
}
