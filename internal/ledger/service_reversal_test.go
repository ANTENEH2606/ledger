package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ledger/internal/db"
)

func postPair(t *testing.T, svc *Service, a, b int64, amount int64, date time.Time) PostResult {
	t.Helper()
	res, err := svc.PostTransaction(context.Background(), PostInput{
		IdempotencyKey: uniqueKey("orig"),
		EffectiveDate:  date,
		Entries:        []EntryInput{{AccountID: a, Amount: amount}, {AccountID: b, Amount: -amount}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReverse_UndoesTheOriginal(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")
	day := time.Now()

	orig := postPair(t, svc, a.ID, b.ID, 700, day)
	rev, err := svc.ReverseTransaction(ctx, orig.ID, ReverseInput{IdempotencyKey: uniqueKey("rev"), EffectiveDate: day})
	if err != nil {
		t.Fatal(err)
	}

	if !rev.ReversesTransactionID.Valid || rev.ReversesTransactionID.Int64 != orig.ID {
		t.Errorf("reversal does not point at the original: %+v", rev.ReversesTransactionID)
	}
	if got := accountBalance(t, svc, a.ID); got != 0 {
		t.Errorf("account a balance = %d, want 0", got)
	}
	if got := accountBalance(t, svc, b.ID); got != 0 {
		t.Errorf("account b balance = %d, want 0", got)
	}

	entries, err := q.ListEntriesByTransaction(ctx, rev.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("reversal entries: %v (%d)", err, len(entries))
	}
}

func TestReverse_OnlyOnce(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")
	day := time.Now()

	orig := postPair(t, svc, a.ID, b.ID, 100, day)
	if _, err := svc.ReverseTransaction(ctx, orig.ID, ReverseInput{IdempotencyKey: uniqueKey("rev1"), EffectiveDate: day}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ReverseTransaction(ctx, orig.ID, ReverseInput{IdempotencyKey: uniqueKey("rev2"), EffectiveDate: day})
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("second reversal: got %v, want ErrAlreadyReversed", err)
	}
	if got := accountBalance(t, svc, a.ID); got != 0 {
		t.Errorf("balance = %d after a refused second reversal, want 0", got)
	}
}

func TestReverse_RetryWithSameKeyIsAReplay(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")
	day := time.Now()

	orig := postPair(t, svc, a.ID, b.ID, 100, day)
	in := ReverseInput{IdempotencyKey: uniqueKey("revretry"), EffectiveDate: day}
	first, err := svc.ReverseTransaction(ctx, orig.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.ReverseTransaction(ctx, orig.ID, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !second.Replayed || second.ID != first.ID {
		t.Errorf("retry: id %d replayed=%v, want id %d replayed=true", second.ID, second.Replayed, first.ID)
	}
	if n := len(eventsFor(t, q, first.ID)); n != 1 {
		t.Errorf("%d events for the reversal, want 1", n)
	}
}

func TestReverse_CannotReverseAReversal(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")
	day := time.Now()

	orig := postPair(t, svc, a.ID, b.ID, 100, day)
	rev, err := svc.ReverseTransaction(ctx, orig.ID, ReverseInput{IdempotencyKey: uniqueKey("rev"), EffectiveDate: day})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ReverseTransaction(ctx, rev.ID, ReverseInput{IdempotencyKey: uniqueKey("revrev"), EffectiveDate: day})
	if !errors.Is(err, ErrCannotReverseReversal) {
		t.Fatalf("got %v, want ErrCannotReverseReversal", err)
	}
}

func TestReverse_UnknownTransaction(t *testing.T) {
	svc, _ := stressEnv(t)
	_, err := svc.ReverseTransaction(context.Background(), 999999999,
		ReverseInput{IdempotencyKey: uniqueKey("revnone"), EffectiveDate: time.Now()})
	if !errors.Is(err, ErrTransactionNotFound) {
		t.Fatalf("got %v, want ErrTransactionNotFound", err)
	}
}

// The overdraft rule applies to reversals too: undoing a deposit whose money was
// already withdrawn would push the wallet below zero.
func TestReverse_RefusedWhenItWouldOverdraw(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	wallet := newTypedAccount(t, q, "asset", "ETB")
	other := newAccount(t, q, "ETB")
	if err := q.SetAllowOverdraft(ctx, db.SetAllowOverdraftParams{ID: wallet.ID, AllowOverdraft: false}); err != nil {
		t.Fatal(err)
	}
	day := time.Now()

	deposit := postPair(t, svc, wallet.ID, other.ID, 100, day) // wallet +100
	_ = postPair(t, svc, wallet.ID, other.ID, -100, day)       // wallet back to 0

	_, err := svc.ReverseTransaction(ctx, deposit.ID, ReverseInput{IdempotencyKey: uniqueKey("revod"), EffectiveDate: day})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
}

func TestReverse_EventPointsAtTheOriginal(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")
	day := time.Now()

	orig := postPair(t, svc, a.ID, b.ID, 100, day)
	rev, err := svc.ReverseTransaction(ctx, orig.ID, ReverseInput{IdempotencyKey: uniqueKey("revev"), EffectiveDate: day})
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsFor(t, q, rev.ID)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	var p postedEvent
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.ReversesTransactionID != orig.ID {
		t.Errorf("event reverses_transaction_id = %d, want %d", p.ReversesTransactionID, orig.ID)
	}
}
