package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/db"
)

// Publisher delivers one event to the outside world.
// Returning an error means "not delivered; try again later".
type Publisher interface {
	Publish(ctx context.Context, ev db.OutboxEvent) error
}

// LogPublisher just logs the event. It is the first, simplest Publisher.
type LogPublisher struct{}

func (LogPublisher) Publish(_ context.Context, ev db.OutboxEvent) error {
	log.Printf("outbox event %d (%s): %s", ev.ID, ev.EventType, ev.Payload)
	return nil
}

type Worker struct {
	pool  *pgxpool.Pool
	pub   Publisher
	batch int
}

func NewWorker(pool *pgxpool.Pool, pub Publisher, batch int) *Worker {
	return &Worker{pool: pool, pub: pub, batch: batch}
}

// RunOnce handles one batch and returns how many events were published.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	q := db.New(tx)

	events, err := q.ClaimOutboxBatch(ctx, int32(w.batch))
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}

	published := 0
	for _, ev := range events {
		if err := w.pub.Publish(ctx, ev); err != nil {
			if mErr := q.MarkOutboxFailed(ctx, db.MarkOutboxFailedParams{
				ID:        ev.ID,
				LastError: pgtype.Text{String: err.Error(), Valid: true},
			}); mErr != nil {
				return 0, fmt.Errorf("mark failed: %w", mErr)
			}
			continue
		}
		if err := q.MarkOutboxPublished(ctx, ev.ID); err != nil {
			return 0, fmt.Errorf("mark published: %w", err)
		}
		published++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return published, nil
}

// Run keeps handling batches until ctx is cancelled. It drains whatever is
// waiting, then sleeps for interval.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			n, err := w.RunOnce(ctx)
			if err != nil {
				if ctx.Err() == nil { // a cancelled context is a normal shutdown, not an error
					log.Printf("outbox: %v", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
