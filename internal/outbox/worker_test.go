package outbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
	"ledger/internal/ledger"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// recorder counts how many times each event id was published.
type recorder struct {
	mu    sync.Mutex
	calls map[int64]int
	delay time.Duration
}

func newRecorder(delay time.Duration) *recorder {
	return &recorder{calls: map[int64]int{}, delay: delay}
}

func (r *recorder) Publish(_ context.Context, ev db.OutboxEvent) error {
	time.Sleep(r.delay) // long enough for another worker to overlap
	r.mu.Lock()
	r.calls[ev.ID]++
	r.mu.Unlock()
	return nil
}

func (r *recorder) count(id int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

type failing struct{}

func (failing) Publish(context.Context, db.OutboxEvent) error { return errors.New("boom") }

func drain(t *testing.T, w *Worker) {
	t.Helper()
	for i := 0; i < 200; i++ {
		n, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("outbox did not drain")
}

// postN posts n transactions and returns the outbox events they created.
func postN(t *testing.T, pool *pgxpool.Pool, n int) []db.OutboxEvent {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	svc := ledger.NewService(pool)

	mk := func(tag string) db.Account {
		code := fmt.Sprintf("OB%s%d", tag, time.Now().UnixNano())
		a, err := q.CreateAccount(ctx, db.CreateAccountParams{
			Code: code, Name: code, Type: "asset", Currency: "ETB", AllowOverdraft: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, b := mk("a"), mk("b")

	var events []db.OutboxEvent
	for i := 0; i < n; i++ {
		res, err := svc.PostTransaction(ctx, ledger.PostInput{
			IdempotencyKey: fmt.Sprintf("obw-%d-%d", time.Now().UnixNano(), i),
			EffectiveDate:  time.Now(),
			Entries: []ledger.EntryInput{
				{AccountID: a.ID, Amount: 100},
				{AccountID: b.ID, Amount: -100},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		evs, err := q.ListOutboxEventsByAggregate(ctx, db.ListOutboxEventsByAggregateParams{
			AggregateType: "transaction", AggregateID: res.ID,
		})
		if err != nil || len(evs) != 1 {
			t.Fatalf("expected one event for transaction %d: %v (%d)", res.ID, err, len(evs))
		}
		events = append(events, evs[0])
	}
	return events
}

func reload(t *testing.T, pool *pgxpool.Pool, ev db.OutboxEvent) db.OutboxEvent {
	t.Helper()
	evs, err := db.New(pool).ListOutboxEventsByAggregate(context.Background(), db.ListOutboxEventsByAggregateParams{
		AggregateType: ev.AggregateType, AggregateID: ev.AggregateID,
	})
	if err != nil || len(evs) != 1 {
		t.Fatalf("reload event %d: %v", ev.ID, err)
	}
	return evs[0]
}

func TestWorker_PublishesAndMarksEvents(t *testing.T) {
	pool := testPool(t)
	rec := newRecorder(0)
	w := NewWorker(pool, rec, 1000)

	events := postN(t, pool, 3)
	drain(t, w)

	for _, ev := range events {
		if got := rec.count(ev.ID); got != 1 {
			t.Errorf("event %d published %d times, want 1", ev.ID, got)
		}
		after := reload(t, pool, ev)
		if !after.PublishedAt.Valid {
			t.Errorf("event %d is not marked published", ev.ID)
		}
		if after.Attempts != 1 {
			t.Errorf("event %d attempts = %d, want 1", ev.ID, after.Attempts)
		}
	}
}

func TestWorker_FailureIsRecordedAndRetried(t *testing.T) {
	pool := testPool(t)
	good := NewWorker(pool, newRecorder(0), 1000)
	drain(t, good) // clear leftovers from earlier runs

	ev := postN(t, pool, 1)[0]

	bad := NewWorker(pool, failing{}, 1000)
	if _, err := bad.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := reload(t, pool, ev)
	if after.PublishedAt.Valid {
		t.Fatalf("a failed event must stay unpublished")
	}
	if after.Attempts != 1 || !after.LastError.Valid || !strings.Contains(after.LastError.String, "boom") {
		t.Errorf("failure not recorded: attempts=%d last_error=%+v", after.Attempts, after.LastError)
	}

	drain(t, good) // the retry succeeds
	after = reload(t, pool, ev)
	if !after.PublishedAt.Valid || after.Attempts != 2 || after.LastError.Valid {
		t.Errorf("after retry: published=%v attempts=%d last_error=%+v",
			after.PublishedAt.Valid, after.Attempts, after.LastError)
	}
}

// Four workers run at once. Every event must be published exactly once.
func TestWorker_ConcurrentWorkersNeverDoublePublish(t *testing.T) {
	pool := testPool(t)
	events := postN(t, pool, 40)

	rec := newRecorder(5 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drain(t, NewWorker(pool, rec, 5))
		}()
	}
	wg.Wait()

	for _, ev := range events {
		if got := rec.count(ev.ID); got != 1 {
			t.Errorf("event %d published %d times, want exactly 1", ev.ID, got)
		}
	}
}
