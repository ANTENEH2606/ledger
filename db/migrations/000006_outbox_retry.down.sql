DROP INDEX IF EXISTS outbox_events_due_idx;
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;
ALTER TABLE outbox_events DROP COLUMN failed_at, DROP COLUMN next_attempt_at;