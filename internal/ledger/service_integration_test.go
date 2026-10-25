package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
)

// These tests need a real database. They are skipped unless DATABASE_URL is set:
//
//	DATABASE_URL="postgres://..." go test ./internal/ledger -count=1
//
// The ledger is append-only, so these tests leave rows behind. Point them at
// a throwaway database, not one you care about.

func testEnv(t *testing.T) (*Service, *db.Queries) {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewService(pool), db.New(pool)
}

// newAccount creates an account with a unique code so tests never collide.
func newAccount(t *testing.T, q *db.Queries, currency string) db.Account {
	t.Helper()

	code := fmt.Sprintf("T%d", time.Now().UnixNano())
	a, err := q.CreateAccount(context.Background(), db.CreateAccountParams{
		Code:     code,
		Name:     "test account " + code,
		Type:     "asset",
		Currency: currency,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

func uniqueKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func TestPostTransaction_Posts(t *testing.T) {
	svc, q := testEnv(t)
	ctx := context.Background()

	cash := newAccount(t, q, "ETB")
	deposits := newAccount(t, q, "ETB")

	tx, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: uniqueKey("posts"),
		EffectiveDate:  time.Now(),
		Description:    "customer deposit",
		Entries: []EntryInput{
			{AccountID: cash.ID, Amount: 100000},
			{AccountID: deposits.ID, Amount: -100000},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	entries, err := q.ListEntriesByTransaction(ctx, tx.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
}

func TestPostTransaction_IdempotentRetry(t *testing.T) {
	svc, q := testEnv(t)
	ctx := context.Background()

	cash := newAccount(t, q, "ETB")
	deposits := newAccount(t, q, "ETB")

	in := PostInput{
		IdempotencyKey: uniqueKey("retry"),
		EffectiveDate:  time.Now(),
		Entries: []EntryInput{
			{AccountID: cash.ID, Amount: 5000},
			{AccountID: deposits.ID, Amount: -5000},
		},
	}

	first, err := svc.PostTransaction(ctx, in)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	second, err := svc.PostTransaction(ctx, in) // same key: a network retry
	if err != nil {
		t.Fatalf("second post: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("retry created a new transaction: %d vs %d", first.ID, second.ID)
	}
	entries, err := q.ListEntriesByTransaction(ctx, first.ID)
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries after retry, want 2", len(entries))
	}
}

func TestPostTransaction_UnknownAccount(t *testing.T) {
	svc, q := testEnv(t)
	cash := newAccount(t, q, "ETB")

	_, err := svc.PostTransaction(context.Background(), PostInput{
		IdempotencyKey: uniqueKey("unknown"),
		EffectiveDate:  time.Now(),
		Entries: []EntryInput{
			{AccountID: cash.ID, Amount: 100},
			{AccountID: 999999999, Amount: -100},
		},
	})
	if !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("got %v, want ErrUnknownAccount", err)
	}
}

func TestPostTransaction_MixedCurrency(t *testing.T) {
	svc, q := testEnv(t)
	etb := newAccount(t, q, "ETB")
	usd := newAccount(t, q, "USD")

	_, err := svc.PostTransaction(context.Background(), PostInput{
		IdempotencyKey: uniqueKey("mixed"),
		EffectiveDate:  time.Now(),
		Entries: []EntryInput{
			{AccountID: etb.ID, Amount: 100},
			{AccountID: usd.ID, Amount: -100},
		},
	})
	if !errors.Is(err, ErrMixedCurrency) {
		t.Fatalf("got %v, want ErrMixedCurrency", err)
	}
}

func TestPostTransaction_KeyReuseWithDifferentPayload(t *testing.T) {
	svc, q := testEnv(t)
	ctx := context.Background()

	a := newAccount(t, q, "ETB")
	b := newAccount(t, q, "ETB")
	key := uniqueKey("reuse")

	post := func(amount int64) (db.Transaction, error) {
		return svc.PostTransaction(ctx, PostInput{
			IdempotencyKey: key,
			EffectiveDate:  time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC),
			Entries: []EntryInput{
				{AccountID: a.ID, Amount: -amount},
				{AccountID: b.ID, Amount: amount},
			},
		})
	}

	// 1. First request succeeds.
	first, err := post(100)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	// 2. Same key, different amount: must be refused.
	if _, err := post(500); !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Fatalf("got %v, want ErrIdempotencyKeyReuse", err)
	}

	// 3. Nothing from the refused request was written.
	var bal int64
	err = svc.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount),0)::bigint FROM entries WHERE account_id = $1`, b.ID).Scan(&bal)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != 100 {
		t.Errorf("balance of B = %d, want 100", bal)
	}

	// 4. A genuine retry of the original request still works.
	again, err := post(100)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("retry returned transaction %d, want %d", again.ID, first.ID)
	}
}
