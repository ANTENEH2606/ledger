-- When the event may next be tried (the retry delay), and when we gave up on it.
ALTER TABLE outbox_events
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN failed_at       TIMESTAMPTZ;

-- The worker only wants events that are unsent, not given up on, and due.
DROP INDEX outbox_events_unpublished_idx;
CREATE INDEX outbox_events_due_idx ON outbox_events (next_attempt_at, id)
    WHERE published_at IS NULL AND failed_at IS NULL;