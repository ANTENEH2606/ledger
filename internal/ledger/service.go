package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
)

var (
	ErrUnknownAccount = errors.New("one or more accounts do not exist")
	ErrMixedCurrency  = errors.New("all accounts in a transaction must share one currency")

	ErrInsufficientFunds     = errors.New("insufficient funds")
	ErrIdempotencyKeyReuse   = errors.New("idempotency key already used with a different request")
	ErrBackdated             = errors.New("posting is dated before the newest entry on a no-overdraft account")
	ErrTransactionNotFound   = errors.New("transaction not found")
	ErrAlreadyReversed       = errors.New("transaction has already been reversed")
	ErrCannotReverseReversal = errors.New("a reversal cannot be reversed; post a new transaction instead")
)

const maxAttempts = 3

// PostResult is the posted transaction plus whether this call was a replay
// of an earlier request with the same idempotency key.
type PostResult struct {
	db.Transaction
	Replayed bool
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Ping checks that the database is reachable.
func (s *Service) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// PostTransaction validates the request and writes the transaction and all
// its entries atomically. Posting the same idempotency key again returns the
// original transaction and writes nothing new.
//
// Deadlocks (40P01) and serialization failures (40001) are retried a few
// times; every other error is returned immediately.
func (s *Service) PostTransaction(ctx context.Context, in PostInput) (PostResult, error) {
	if err := validate(in); err != nil {
		return PostResult{}, err
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
			return PostResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
	return PostResult{}, fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}

// post performs one attempt, inside a single database transaction.
func (s *Service) post(ctx context.Context, in PostInput) (PostResult, error) {
	hash := requestHash(in)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PostResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // harmless after a successful Commit

	q := db.New(tx)

	// 1. Lock every account involved, in id order, before writing anything.
	//    Same order for everyone means no deadlocks.
	ids := accountIDs(in.Entries)
	accounts, err := q.LockAccountsForUpdate(ctx, ids)
	if err != nil {
		return PostResult{}, fmt.Errorf("lock accounts: %w", err)
	}
	if len(accounts) != len(ids) {
		return PostResult{}, ErrUnknownAccount
	}
	for _, a := range accounts[1:] {
		if a.Currency != accounts[0].Currency {
			return PostResult{}, ErrMixedCurrency
		}
	}

	// 2. Insert the transaction header. If the idempotency key already
	//    exists, ON CONFLICT DO NOTHING returns no row: this is a retry.
	t, err := q.CreateTransaction(ctx, db.CreateTransactionParams{
		IdempotencyKey:        in.IdempotencyKey,
		EffectiveDate:         in.EffectiveDate,
		Description:           in.Description,
		ReversesTransactionID: pgtype.Int8{Int64: in.ReversesTransactionID, Valid: in.ReversesTransactionID != 0},
		RequestHash:           hash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := q.GetTransactionByIdempotencyKey(ctx, in.IdempotencyKey)
		if getErr != nil {
			return PostResult{}, fmt.Errorf("load original transaction: %w", getErr)
		}
		if existing.RequestHash != "" && existing.RequestHash != hash {
			return PostResult{}, ErrIdempotencyKeyReuse
		}
		return PostResult{Transaction: existing, Replayed: true}, nil
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
			pgErr.ConstraintName == "transactions_reverses_transaction_id_key" {
			return PostResult{}, ErrAlreadyReversed
		}
		return PostResult{}, fmt.Errorf("create transaction: %w", err)
	}

	// 3. Enforce "no overdraft" on accounts that forbid it. This reads
	//    balances, which is exactly why the locks above matter: while we hold
	//    them, nobody else can change these balances under us.
	if err := checkFunds(ctx, q, accounts, in.Entries, in.EffectiveDate); err != nil {
		return PostResult{}, err
	}

	// 4a. Insert the entries.
	for _, e := range in.Entries {
		_, err := q.CreateEntry(ctx, db.CreateEntryParams{
			TransactionID: t.ID,
			AccountID:     e.AccountID,
			Amount:        e.Amount,
		})
		if err != nil {
			return PostResult{}, fmt.Errorf("create entry: %w", err)
		}
	}

	// 4b. Record the event in the SAME database transaction. If anything fails
	//     (or the commit is rejected), the event disappears together with the posting.
	ev := postedEvent{
		TransactionID:         t.ID,
		ReversesTransactionID: in.ReversesTransactionID, // new: zero for normal postings
		EffectiveDate:         in.EffectiveDate.Format("2006-01-02"),
		Description:           in.Description,
	}
	for _, e := range in.Entries {
		ev.Entries = append(ev.Entries, postedEntry{AccountID: e.AccountID, Amount: e.Amount})
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return PostResult{}, fmt.Errorf("marshal event: %w", err)
	}
	if _, err := q.CreateOutboxEvent(ctx, db.CreateOutboxEventParams{
		AggregateType: "transaction",
		AggregateID:   t.ID,
		EventType:     "transaction.posted",
		Payload:       payload,
	}); err != nil {
		return PostResult{}, fmt.Errorf("create outbox event: %w", err)
	}

	// 5. Commit. The deferred balance trigger runs here and rejects the
	//    whole transaction if the entries do not add up.
	if err := tx.Commit(ctx); err != nil {
		return PostResult{}, fmt.Errorf("commit: %w", err)
	}
	return PostResult{Transaction: t, Replayed: false}, nil
}

// checkFunds refuses the posting if it would push an account that forbids
// overdraft onto the wrong side of zero. The accounts must already be locked.
func checkFunds(ctx context.Context, q *db.Queries, accounts []db.LockAccountsForUpdateRow, entries []EntryInput, date time.Time) error {
	delta := make(map[int64]int64)
	for _, e := range entries {
		delta[e.AccountID] += e.Amount
	}

	for _, a := range accounts {
		if a.AllowOverdraft {
			continue
		}
		// Backdating would let a past date go negative even though "now" looks fine.
		later, err := q.HasEntriesAfter(ctx, db.HasEntriesAfterParams{
			AccountID: a.ID,
			OnDate:    date,
		})
		if err != nil {
			return fmt.Errorf("check dates of account %d: %w", a.ID, err)
		}
		if later {
			return fmt.Errorf("%w: account %d", ErrBackdated, a.ID)
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

type postedEntry struct {
	AccountID int64 `json:"account_id"`
	Amount    int64 `json:"amount"`
}

type postedEvent struct {
	TransactionID         int64         `json:"transaction_id"`
	ReversesTransactionID int64         `json:"reverses_transaction_id,omitempty"`
	EffectiveDate         string        `json:"effective_date"`
	Description           string        `json:"description"`
	Entries               []postedEntry `json:"entries"`
}

type ReverseInput struct {
	IdempotencyKey string
	EffectiveDate  time.Time
	Description    string // optional
}

// ReverseTransaction posts a new transaction that exactly undoes an earlier one.
// Posted rows never change, so reading the original outside the posting transaction is safe.
func (s *Service) ReverseTransaction(ctx context.Context, originalID int64, in ReverseInput) (PostResult, error) {
	q := db.New(s.pool)

	orig, err := q.GetTransaction(ctx, originalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PostResult{}, ErrTransactionNotFound
	}
	if err != nil {
		return PostResult{}, fmt.Errorf("load transaction: %w", err)
	}
	if orig.ReversesTransactionID.Valid {
		return PostResult{}, ErrCannotReverseReversal
	}

	entries, err := q.ListEntriesByTransaction(ctx, originalID)
	if err != nil {
		return PostResult{}, fmt.Errorf("load entries: %w", err)
	}

	flipped := make([]EntryInput, len(entries))
	for i, e := range entries {
		if e.Amount == math.MinInt64 { // -MinInt64 does not fit in an int64
			return PostResult{}, ErrOverflow
		}
		flipped[i] = EntryInput{AccountID: e.AccountID, Amount: -e.Amount}
	}

	desc := in.Description
	if desc == "" {
		desc = fmt.Sprintf("Reversal of transaction %d", originalID)
	}

	return s.PostTransaction(ctx, PostInput{
		IdempotencyKey:        in.IdempotencyKey,
		EffectiveDate:         in.EffectiveDate,
		Description:           desc,
		Entries:               flipped,
		ReversesTransactionID: originalID,
	})
}
