package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"ledger/internal/db"
)

func eventsFor(t *testing.T, q *db.Queries, txID int64) []db.OutboxEvent {
	t.Helper()
	evs, err := q.ListOutboxEventsByAggregate(context.Background(), db.ListOutboxEventsByAggregateParams{
		AggregateType: "transaction",
		AggregateID:   txID,
	})
	if err != nil {
		t.Fatalf("list outbox events: %v", err)
	}
	return evs
}

func TestOutbox_PostingWritesOneEvent(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")

	res, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: uniqueKey("ob"),
		EffectiveDate:  time.Now(),
		Description:    "outbox test",
		Entries:        []EntryInput{{AccountID: a.ID, Amount: 100}, {AccountID: b.ID, Amount: -100}},
	})
	if err != nil {
		t.Fatal(err)
	}

	evs := eventsFor(t, q, res.ID)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.EventType != "transaction.posted" {
		t.Errorf("event type = %q, want transaction.posted", ev.EventType)
	}
	if ev.PublishedAt.Valid {
		t.Errorf("new event is already marked published")
	}
	if ev.Attempts != 0 {
		t.Errorf("attempts = %d, want 0", ev.Attempts)
	}

	var p postedEvent
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if p.TransactionID != res.ID {
		t.Errorf("payload transaction_id = %d, want %d", p.TransactionID, res.ID)
	}
	if len(p.Entries) != 2 {
		t.Fatalf("payload has %d entries, want 2", len(p.Entries))
	}
	var sum int64
	for _, e := range p.Entries {
		sum += e.Amount
	}
	if sum != 0 {
		t.Errorf("payload entries sum to %d, want 0", sum)
	}
}

func TestOutbox_ReplayWritesNoSecondEvent(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()
	a, b := newAccount(t, q, "ETB"), newAccount(t, q, "ETB")

	in := PostInput{
		IdempotencyKey: uniqueKey("obreplay"),
		EffectiveDate:  time.Now(),
		Entries:        []EntryInput{{AccountID: a.ID, Amount: 100}, {AccountID: b.ID, Amount: -100}},
	}
	first, err := svc.PostTransaction(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.PostTransaction(ctx, in) // a network retry
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.ID != first.ID {
		t.Fatalf("second call should be a replay of %d, got id %d replayed=%v", first.ID, second.ID, second.Replayed)
	}
	if n := len(eventsFor(t, q, first.ID)); n != 1 {
		t.Errorf("after a replay there are %d events, want 1", n)
	}
}

// A posting that is refused leaves no event behind: the event lives in the same
// database transaction, so it is rolled back together with everything else.
func TestOutbox_RejectedPostingWritesNoEvent(t *testing.T) {
	svc, q := stressEnv(t)
	ctx := context.Background()

	wallet := newTypedAccount(t, q, "asset", "ETB")
	other := newAccount(t, q, "ETB")
	if err := q.SetAllowOverdraft(ctx, db.SetAllowOverdraftParams{ID: wallet.ID, AllowOverdraft: false}); err != nil {
		t.Fatal(err)
	}

	// The wallet is empty, so crediting it (a withdrawal) must be refused.
	_, err := svc.PostTransaction(ctx, PostInput{
		IdempotencyKey: uniqueKey("obreject"),
		EffectiveDate:  time.Now(),
		Entries:        []EntryInput{{AccountID: other.ID, Amount: 100}, {AccountID: wallet.ID, Amount: -100}},
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}

	// Find any event that mentions this wallet. There must be none.
	var n int
	err = svc.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE payload->'entries' @> $1::jsonb`,
		fmt.Sprintf(`[{"account_id": %d}]`, wallet.ID)).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("found %d events for a rejected posting, want 0", n)
	}
}
