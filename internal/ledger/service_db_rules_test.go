package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Raw SQL, bypassing the Go service: the database itself refuses an unbalanced transaction.
func TestDB_RejectsUnbalancedRawInsert(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	var txID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO transactions (idempotency_key, effective_date, description)
		 VALUES ($1, CURRENT_DATE, 'raw') RETURNING id`, uniqueKey("raw")).Scan(&txID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO entries (transaction_id, account_id, amount) VALUES ($1,$2,100), ($1,$3,-90)`,
		txID, a.ID, b.ID); err != nil {
		t.Fatal(err) // the balance check is deferred to COMMIT
	}

	if err := tx.Commit(ctx); pgCode(err) != "P0001" {
		t.Fatalf("commit: got %v, want error code P0001", err)
	}
}

// A posted transaction cannot be changed or removed, even with raw SQL.
func TestDB_RejectsUpdateAndDelete(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")

	posted, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: uniqueKey("ro"),
		EffectiveDate:  time.Now(),
		Entries:        []EntryInput{{AccountID: a.ID, Amount: -100}, {AccountID: b.ID, Amount: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}

	statements := []string{
		`UPDATE entries SET amount = 1 WHERE transaction_id = $1`,
		`DELETE FROM entries WHERE transaction_id = $1`,
		`UPDATE transactions SET description = 'x' WHERE id = $1`,
		`DELETE FROM transactions WHERE id = $1`,
	}
	for _, sql := range statements {
		if _, err := svc.pool.Exec(ctx, sql, posted.ID); pgCode(err) != "P0001" {
			t.Errorf("%q: got %v, want error code P0001", sql, err)
		}
	}
}

// TRUNCATE must be refused too. It runs in a transaction that is never committed,
// so even if the protection were missing, no data would be lost.
func TestDB_RejectsTruncate(t *testing.T) {
	svc, _ := stressEnv(t)
	ctx := context.Background()

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Both tables listed together, because transactions is referenced by a foreign key.
	if _, err := tx.Exec(ctx, `TRUNCATE entries, transactions`); pgCode(err) != "P0001" {
		t.Fatalf("truncate: got %v, want error code P0001", err)
	}
}
