package ledger

import (
	"context"
	"testing"
	"time"
)

func TestGetTrialBalance_BackdatedZeroBalance(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()

	acc := newAccount(t, q, "ETB")
	counterparty := newAccount(t, q, "ETB")

	// Post a future transaction
	futureDate := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: uniqueKey("future-tx"),
		EffectiveDate:  futureDate,
		Entries: []EntryInput{
			{AccountID: acc.ID, Amount: 100},
			{AccountID: counterparty.ID, Amount: -100},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Request trial balance for an earlier as_of date
	pastDate := time.Now().AddDate(-1, 0, 0)
	balances, err := q.GetTrialBalance(ctx, pastDate)
	if err != nil {
		t.Fatal(err)
	}

	// Assert account is present with 0 balance
	found := false
	for _, b := range balances {
		if b.ID == acc.ID {
			found = true
			if b.Balance != 0 {
				t.Errorf("got balance %d, want 0", b.Balance)
			}
		}
	}
	if !found {
		t.Errorf("account %d missing from trial balance for past date", acc.ID)
	}
}
