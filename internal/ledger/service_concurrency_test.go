package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
)

// Stress tests: many goroutines posting at the same moment.
// Like the other integration tests, they are skipped unless DATABASE_URL is set.
// Run them with the race detector too:
//
//	DATABASE_URL=... go test ./internal/ledger -run Concurrent -race -count=1 -v

// stressEnv is like testEnv but with a bigger connection pool, so many
// postings really do run in parallel.
func stressEnv(t *testing.T) (*Service, *db.Queries) {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 20

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewService(pool), db.New(pool)
}

// accountBalance returns the signed balance of an account:
// positive = net debit, negative = net credit.
func accountBalance(t *testing.T, svc *Service, accountID int64) int64 {
	t.Helper()

	var balance int64
	err := svc.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE account_id = $1`,
		accountID).Scan(&balance)
	if err != nil {
		t.Fatalf("balance of account %d: %v", accountID, err)
	}
	return balance
}

// transfer posts `amount` from one account to another (credit from, debit to).
func transfer(svc *Service, key string, from, to, amount int64) error {
	_, err := svc.PostTransaction(context.Background(), PostInput{
		IdempotencyKey: key,
		EffectiveDate:  time.Now(),
		Description:    "stress transfer",
		Entries: []EntryInput{
			{AccountID: from, Amount: -amount},
			{AccountID: to, Amount: amount},
		},
	})
	return err
}

// runConcurrently starts n goroutines at the same moment and waits for all of them.
func runConcurrently(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})

	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start // everyone waits here, then all go at once
			fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// 100 transfers of 100 from A to B at the same time: nothing lost, nothing doubled.
func TestConcurrent_SameDirection(t *testing.T) {
	svc, q := stressEnv(t)

	a := newAccount(t, q, "ETB")
	b := newAccount(t, q, "ETB")
	run := uniqueKey("same")

	const n, amount = 100, 100
	runConcurrently(n, func(i int) {
		if err := transfer(svc, fmt.Sprintf("%s-%d", run, i), a.ID, b.ID, amount); err != nil {
			t.Errorf("transfer %d: %v", i, err)
		}
	})

	if got, want := accountBalance(t, svc, a.ID), int64(-n*amount); got != want {
		t.Errorf("account A balance = %d, want %d", got, want)
	}
	if got, want := accountBalance(t, svc, b.ID), int64(n*amount); got != want {
		t.Errorf("account B balance = %d, want %d", got, want)
	}
}

// Transfers in BOTH directions at once. Without ordered locking this is the
// classic deadlock. With it, every transfer succeeds and the balances net to zero.
func TestConcurrent_OppositeDirections(t *testing.T) {
	svc, q := stressEnv(t)

	a := newAccount(t, q, "ETB")
	b := newAccount(t, q, "ETB")
	run := uniqueKey("opp")

	const n, amount = 100, 100
	runConcurrently(n, func(i int) {
		from, to := a.ID, b.ID
		if i%2 == 1 {
			from, to = b.ID, a.ID
		}
		if err := transfer(svc, fmt.Sprintf("%s-%d", run, i), from, to, amount); err != nil {
			t.Errorf("transfer %d: %v", i, err)
		}
	})

	// 50 each way cancels out exactly.
	if got := accountBalance(t, svc, a.ID); got != 0 {
		t.Errorf("account A balance = %d, want 0", got)
	}
	if got := accountBalance(t, svc, b.ID); got != 0 {
		t.Errorf("account B balance = %d, want 0", got)
	}
}

// The same request arriving many times at once (impatient client, flaky network):
// exactly one transaction is created and everyone gets the same answer.
func TestConcurrent_SameIdempotencyKey(t *testing.T) {
	svc, q := stressEnv(t)

	a := newAccount(t, q, "ETB")
	b := newAccount(t, q, "ETB")
	key := uniqueKey("idem")

	const n = 30
	ids := make([]int64, n)
	runConcurrently(n, func(i int) {
		tx, err := svc.PostTransaction(context.Background(), PostInput{
			IdempotencyKey: key,
			EffectiveDate:  time.Now(),
			Entries: []EntryInput{
				{AccountID: a.ID, Amount: -250},
				{AccountID: b.ID, Amount: 250},
			},
		})
		if err != nil {
			t.Errorf("post %d: %v", i, err)
			return
		}
		ids[i] = tx.ID
	})

	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("request %d got transaction %d, request 0 got %d", i, id, ids[0])
		}
	}
	// Posted once, not 30 times.
	if got := accountBalance(t, svc, b.ID); got != 250 {
		t.Errorf("account B balance = %d, want 250", got)
	}
}

// newTypedAccount is like newAccount but lets the test choose the account type.
func newTypedAccount(t *testing.T, q *db.Queries, accountType, currency string) db.Account {
	t.Helper()

	code := fmt.Sprintf("W%d", time.Now().UnixNano())
	a, err := q.CreateAccount(context.Background(), db.CreateAccountParams{
		Code:     code,
		Name:     "test " + accountType + " " + code,
		Type:     accountType,
		Currency: currency,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return a
}

// THIS is the test that needs the account locks.
//
// A customer wallet (a liability, so its normal balance is a credit) holds 1000
// and forbids overdraft. 30 withdrawals of 100 arrive at the same moment.
// Exactly 10 may succeed; the other 20 must be refused.
//
// Without the locks, many requests would read "balance: 1000" at the same time,
// all decide there is enough, and all succeed: the wallet ends up overdrawn.
// Try it: comment out the "Lock every account" block in service.go and rerun.
func TestConcurrent_NoOverdraft(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()

	wallet := newTypedAccount(t, q, "liability", "ETB")
	cash := newTypedAccount(t, q, "asset", "ETB")
	if err := q.SetAllowOverdraft(ctx, db.SetAllowOverdraftParams{
		ID:             wallet.ID,
		AllowOverdraft: false,
	}); err != nil {
		t.Fatalf("forbid overdraft: %v", err)
	}

	// The customer deposits 1000: debit cash, credit the wallet.
	run := uniqueKey("wd")
	_, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: run + "-deposit",
		EffectiveDate:  time.Now(),
		Entries: []EntryInput{
			{AccountID: cash.ID, Amount: 1000},
			{AccountID: wallet.ID, Amount: -1000},
		},
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// 30 simultaneous withdrawals of 100: debit the wallet, credit cash.
	var succeeded atomic.Int64
	runConcurrently(30, func(i int) {
		_, err := svc.PostTransaction(ctx, PostInput{
			IdempotencyKey: fmt.Sprintf("%s-w%d", run, i),
			EffectiveDate:  time.Now(),
			Entries: []EntryInput{
				{AccountID: wallet.ID, Amount: 100},
				{AccountID: cash.ID, Amount: -100},
			},
		})
		switch {
		case err == nil:
			succeeded.Add(1)
		case errors.Is(err, ErrInsufficientFunds):
			// expected for the 20 that arrive after the money is gone
		default:
			t.Errorf("withdrawal %d: unexpected error: %v", i, err)
		}
	})

	if got := succeeded.Load(); got != 10 {
		t.Errorf("%d withdrawals succeeded, want exactly 10", got)
	}
	// The wallet is credit-normal: a balance above zero would mean overdrawn.
	if got := accountBalance(t, svc, wallet.ID); got != 0 {
		t.Errorf("wallet balance = %d, want 0 (positive means overdrawn)", got)
	}
}

// The accountant's check: across the whole ledger, debits equal credits.
func TestTrialBalanceIsZero(t *testing.T) {
	svc, _ := stressEnv(t)

	var total int64
	err := svc.pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(amount), 0)::bigint FROM entries`).Scan(&total)
	if err != nil {
		t.Fatalf("sum entries: %v", err)
	}
	if total != 0 {
		t.Fatalf("trial balance = %d, want 0", total)
	}
}
