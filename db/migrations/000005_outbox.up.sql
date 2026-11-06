-- Events to publish. Written in the same transaction as the posting.
-- Only published_at, attempts and last_error ever change; the rest is write-once.
CREATE TABLE outbox_events (
    id             BIGSERIAL   PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   BIGINT      NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT
);

-- The worker only ever looks at unpublished rows, so index just those.
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;