![CI](https://github.com/ANTENEH2606/ledger/actions/workflows/ci.yml/badge.svg)

# Ledger

A double-entry ledger service written in Go, backed by PostgreSQL. It is built to stay correct under retries and concurrent requests: money is never lost, never double-posted, and every transaction balances. The important rules are enforced by the database itself, not only by application code.

> **Status:** core ledger complete. Atomic posting with idempotency, concurrency-safe overdraft protection, reversals, an HTTP API, reports (paginated statement, balance as of a date, trial balance), and a transactional outbox that delivers signed webhooks. Tested on every push by CI, including concurrency tests under Go's race detector and tests that attack the database triggers with raw SQL. No authentication yet (see [Known limits](#known-limits)).

New here? Start with the [one-page overview](docs/OVERVIEW.md).

## What it guarantees

| Guarantee | How it is enforced |
| --- | --- |
| **Every transaction balances** | A deferred constraint trigger checks at `COMMIT` that a transaction has at least two entries, in one currency, summing to zero. An unbalanced transaction cannot be committed, even with raw SQL. |
| **History is never rewritten** | `UPDATE`, `DELETE` and `TRUNCATE` on `entries` and `transactions` are blocked by triggers. Mistakes are corrected with reversing transactions (`POST /transactions/:id/reverse`), as in real accounting. |
| **A transaction is reversed at most once** | `reverses_transaction_id` is `UNIQUE`, so the database itself refuses a second reversal, even if two requests race. |
| **Retries never double-post** | `idempotency_key` is `UNIQUE`; the insert uses `ON CONFLICT DO NOTHING`, and a retry returns the original transaction (`200`, with `"replayed": true`). |
| **A reused key with different content is rejected** | A SHA-256 fingerprint of the request (entries, effective date and, for reversals, the original transaction id) is stored with the key. Same key and different content returns `409`. |
| **No lost updates under concurrency** | Posting locks every account involved with `SELECT ... FOR UPDATE`, in id order, so transfers in opposite directions cannot deadlock. Deadlocks and serialization failures are retried. |
| **No overdraft where forbidden** | Accounts with `allow_overdraft = false` are checked against a balance read *while the account is locked*, so simultaneous withdrawals cannot all pass the check. Backdated postings on such accounts are refused, so a past date cannot go negative either. Reversals obey the same rule. |
| **Events are never lost or invented** | Every posting writes a `transaction.posted` event to `outbox_events` in the *same* database transaction. A rejected or rolled-back posting leaves no event; a committed one always has one. A worker delivers events at least once, with retries. |
| **No rounding errors** | Amounts are signed `BIGINT` in minor units (positive = debit, negative = credit). No floats. Sums are overflow-checked. |
| **Balances are derived** | A balance is the sum of entries. It is never stored and overwritten. |

The concurrency test fires 30 simultaneous withdrawals at a wallet holding 1,000 (100 each): exactly 10 succeed and the balance ends at 0. Without the account locks, 24 got through.

## Tech stack

- **Go** with **Gin** for the HTTP API
- **PostgreSQL 16** for storage
- **pgx** (`pgxpool`) as the driver, **sqlc** for type-safe queries
- **golang-migrate** for schema migrations
- **Docker Compose** to run the whole stack, **GitHub Actions** for CI

## Quick start

You need [Docker](https://docs.docker.com/get-docker/) with Compose.

```bash
git clone https://github.com/ANTENEH2606/ledger.git
cd ledger
cp .env.example .env
```

Edit `.env` and choose a password. Use the same one in `PG_PASSWORD` and in both URLs. `.env` is ignored by Git and must never be committed.

```bash
docker compose up -d --build
curl localhost:8080/health
```

This starts PostgreSQL, applies the migrations, and starts the API on `localhost:8080`. Both ports are bound to `127.0.0.1` only.

Try it:

```bash
# two accounts
curl -X POST localhost:8080/accounts -H "Content-Type: application/json" \
  -d '{"code":"1010","name":"Cash","type":"asset","currency":"ETB"}'
curl -X POST localhost:8080/accounts -H "Content-Type: application/json" \
  -d '{"code":"2010","name":"Deposits","type":"liability","currency":"ETB"}'

# post a transaction: debit cash 100.00, credit deposits 100.00 (minor units)
curl -X POST localhost:8080/transactions \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7" \
  -d '{"effective_date":"2026-10-30","description":"Customer deposit",
       "entries":[{"account_id":1,"amount":10000},{"account_id":2,"amount":-10000}]}'
```

Run the same transaction command again: you get the same transaction back (status `200` and `"replayed": true`), and nothing is posted twice.

Made a mistake? Reverse it (use the `id` from the response above, here `1`):

```bash
curl -X POST localhost:8080/transactions/1/reverse \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: 0b8a2d34-5e1c-4f6a-9a77-3c2d1e4f5a6b" \
  -d '{"effective_date":"2026-10-30"}'
```

Both accounts are back to zero, and the original transaction is still there, untouched.

By default the API only logs the events it produces (look for `outbox event ...` in `docker compose logs api`). See [Events and webhooks](#events-and-webhooks) to deliver them to a URL.

## API

| Method and path | Purpose |
| --- | --- |
| `POST /accounts` | Create an account (`code`, `name`, `type`, `currency`, optional `allow_overdraft`, default `true`) |
| `POST /transactions` | Post a transaction. Requires the `Idempotency-Key` header. |
| `POST /transactions/:id/reverse` | Post a new transaction that exactly undoes transaction `:id`. Requires the `Idempotency-Key` header and `{"effective_date": "YYYY-MM-DD"}`; `description` is optional. See [Reversals](#reversals). |
| `GET /accounts/:id/statement?limit=50&cursor=...` | Entries on an account with a running balance, oldest first, one page at a time |
| `GET /accounts/:id/balance?as_of=YYYY-MM-DD` | Balance at the end of a date (default: today) |
| `GET /trial-balance?as_of=YYYY-MM-DD` | Debits, credits and balance per account, with totals per currency |
| `GET /health` | Liveness and database check |

All amounts are integers in minor units. In a transaction, positive is a debit and negative is a credit. The effective date is supplied by the client.

**Pagination.** The statement returns up to `limit` entries (1 to 200, default 50) and a `next_cursor`. Pass it back as `?cursor=` to get the next page. `next_cursor` is `null` on the last page. A cursor (not an offset) keeps pages stable even if entries are added while you page. The running balance always covers the account's whole history, not just the page.

| Status | Meaning |
| --- | --- |
| `200` | The request worked. Reads (statement, balance, trial balance, health), and a replayed `POST /transactions` or reversal where nothing new was written. |
| `201` | Something new was created (`POST /accounts`, or the first post or reversal with a given key). |
| `400` | Malformed request: missing key, fewer than two entries, a zero amount, unbalanced, bad date, bad id, bad `limit` or `cursor`. Also a posting dated before the newest entry of a no-overdraft account. |
| `404` | Account or transaction not found. |
| `409` | Idempotency key reused with different content, duplicate account code, or a transaction that has already been reversed. |
| `422` | Valid request that breaks a business rule: unknown account, mixed currencies, insufficient funds, or reversing a reversal. |
| `500` | Server error (details are never shown). |

## Reversals

Posted rows never change, so mistakes are fixed by posting an opposite transaction, the way accountants do. `POST /transactions/:id/reverse` reads the original entries, flips every amount, links the new transaction to the original (`reverses_transaction_id`), and posts it through the same path as any other transaction.

- **Once only.** A unique constraint on `reverses_transaction_id` means the database itself refuses a second reversal (`409`), even if two requests race.
- **No reversing a reversal** (`422`). To undo a reversal, post a new transaction.
- **Same rules as any posting.** A reversal is refused with `422` if it would overdraw a no-overdraft account (for example, undoing a deposit whose money has already been withdrawn), and with `400` if it is dated before the newest entry of such an account.
- **Idempotent.** Retrying with the same `Idempotency-Key` returns the original reversal with `200` and `"replayed": true`. The request fingerprint includes the original transaction id, so reusing a key for a different transaction is a `409`.
- **Events.** A reversal writes its own `transaction.posted` event, with `reverses_transaction_id` set so receivers can tell it apart from a normal posting.

## Events and webhooks

Every successful posting (including reversals) also writes a `transaction.posted` event into the `outbox_events` table, **inside the same database transaction** as the entries. This is the transactional outbox pattern: saving and then sending as two separate steps can lose an event (a crash in between) or announce money that never moved (the save fails after the send). With the outbox, the event and the posting commit together or not at all.

A background worker inside the API process then delivers the events:

1. It claims a batch of unsent events with `SELECT ... FOR UPDATE SKIP LOCKED`, so several workers (or several API instances) never take the same event.
2. It publishes each one. With no configuration, events are only logged. With `WEBHOOK_URL` and `WEBHOOK_SECRET` set, each event is POSTed to that URL.
3. It marks the event as sent, or records the failure.

**Delivery is at least once, not exactly once.** If the receiver answers but the worker crashes before saving that fact, the event is sent again. Receivers must remember the event ids they have already handled and ignore repeats.

**Retries.** A failed event is retried after 5s, 10s, 20s, and so on, up to 10 minutes between tries. After 8 failed attempts it is marked `failed_at` and no longer retried (a dead letter you can inspect in the table). A response that is not `2xx` counts as a failure, and redirects are not followed.

**Payload** (the body of the POST, exactly as stored):

```json
{"transaction_id": 3847, "effective_date": "2026-11-07", "description": "outbox demo",
 "entries": [{"account_id": 220, "amount": 5000}, {"account_id": 221, "amount": -5000}]}
```

For a reversal, the payload also has `"reverses_transaction_id"` with the id of the original. The field is left out for normal postings.

**Headers:**

| Header | Meaning |
| --- | --- |
| `X-Ledger-Event-Id` | Unique id of the event. Use it to ignore duplicates. |
| `X-Ledger-Event-Type` | `transaction.posted` |
| `X-Ledger-Timestamp` | Unix time when this attempt was sent |
| `X-Ledger-Signature` | `v1=` followed by the hex HMAC-SHA256 of `<timestamp>.<body>` with the shared secret |

**Verifying a request.** Compute the HMAC over the timestamp, a dot, and the **raw body bytes** exactly as received (do not parse and re-serialize the JSON), compare in constant time, and reject old timestamps so a captured request cannot be replayed:

```go
func verify(secret []byte, timestamp, signature string, body []byte) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
```

**Configuration** (environment variables, never stored in the repo):

| Variable | Meaning |
| --- | --- |
| `WEBHOOK_URL` | Where to send events. Unset means events are only logged. |
| `WEBHOOK_SECRET` | Shared secret for the signature, at least 16 characters. Keep it out of Git. |

For example: `WEBHOOK_URL=https://example.com/hooks WEBHOOK_SECRET=<long random value> make run`.

## Design decisions

- **Signed amounts.** One column instead of separate debit and credit columns. "Debits equal credits" becomes `SUM(amount) = 0`, which the database can check directly.
- **Rules in the database.** Application code can have bugs and other writers can exist. Triggers and constraints make the invariants hold for every client, and tests attack them with raw SQL to prove it.
- **Locks only where a rule reads a balance.** Because balances are derived from append-only entries, plain concurrent inserts are safe. Account locks are needed for rules such as "no overdraft", where a check-then-act race would otherwise be possible. Accounts are always locked in id order to rule out deadlocks.
- **A versioned request fingerprint.** The hash covers only what the client sent (sorted entries and the effective date) with a `ledger/request/v1` prefix. It leaves out the description and anything server-generated, so a retry always produces the same hash. For reversals it also covers the original transaction id, and the text of normal postings is unchanged, so stored hashes stay valid. It is never returned by the API.
- **Corrections by reversal.** A reversal is a normal posting with flipped amounts and a link to the original, so it goes through every existing rule (balance, overdraft, idempotency, outbox) with no special case. The database guarantees "at most once" with a unique constraint; the code only turns that failure into a clean `409`.
- **Cursor pagination.** The cursor is the date and id of the last entry returned, so pages do not shift when new entries arrive.
- **Events in the same transaction.** The outbox row is written with the entries, so an event can never exist without its posting, or the other way round. Delivery is a separate, retried step.
- **Signed webhooks.** The signature covers the timestamp and the body, so a request cannot be altered or replayed later. The secret never appears in logs or in stored error messages.
- **Clean shutdown.** On SIGINT or SIGTERM the server stops accepting connections, lets in-flight requests finish (up to 15 seconds), stops the outbox worker, then closes the database pool. It also sets read, write and idle timeouts.

## Development

Requires Go (see `go.mod`), Docker, and the [`migrate`](https://github.com/golang-migrate/migrate) CLI.

```bash
make up                 # start the full stack in Docker
make down               # stop it (data stays in the ledger_data volume)

make migrate-up         # apply migrations to the main database
make psql               # open psql on the main database
make run                # run the API on the host (release mode; use GIN_MODE=debug make run for route logs)

make test-db-create     # once: create the separate ledger_test database
make test-db-migrate    # apply migrations to the test database
make test-db            # run all tests against ledger_test
```

Only one of `make run` and the `api` container can use port `8080` at a time. For daily work, keep just the database in Docker (`docker compose up -d db`) and use `make run`; use the full stack to check the packaged setup.

Tests run against a separate `ledger_test` database so they never touch real data. Database tests are skipped when `DATABASE_URL` is not set, so check for `PASS` and not `SKIP` when running them with `-v`.

After changing a migration or `db/queries/ledger.sql`, run `sqlc generate`. Apply new migrations **before** starting new code: the code expects the new schema.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `connection refused` on port `5433` | The database container is stopped. Run `docker compose up -d db` and wait until `docker compose ps` shows it healthy. |
| `password authentication failed` | Postgres only reads the password when it first creates its data volume. If you changed `PG_PASSWORD` later, either change the database password with `ALTER USER`, or remove the volume (`docker volume rm ledger_data`, **which deletes all data**) and start again. |
| `port is already allocated` or `address already in use` | Another program uses `5433` or `8080` (for example the `api` container while you run `make run`). Stop it, or change the ports in `docker-compose.yml`. |
| `container name "/ledger" is already in use` | An older container with that name exists. Remove it with `docker rm ledger` (without `-v`) and run `docker compose up -d` again. |
| `DATABASE_URL is not set` | Run the command through `make`, which reads `.env`, or export the variables yourself. |
| `outbox: claim: column "next_attempt_at" does not exist` | The database is missing a migration. Run `make migrate-up`. |
| Webhook events keep failing | Check the `last_error` column of `outbox_events`: `connection refused` means the receiver is down, `status 401` usually means a different secret. |

## Known limits

- **No authentication or authorization.** The API listens on `127.0.0.1` only. Idempotency keys are global; a real deployment should scope them per authenticated client.
- **Hot accounts serialize.** Postings that touch the same account queue on its lock.
- **A reversal cannot be reversed**, and a reversal uses the date the caller sends, not the original's date. On a no-overdraft account it can be refused if the money has since left, or if it is dated before a newer entry. There is no pagination on the trial balance.
- **The statement's running balance is recomputed over the account's whole history on every page.** It is correct, but slow for very large accounts; a stored checkpoint balance would fix that.
- **One currency per transaction.** There is no foreign-exchange support.
- **The backdating rule is deliberately conservative.** An account with `allow_overdraft = false` refuses any posting dated before its newest entry, including backdated deposits. Accounts that allow overdraft can be backdated freely.
- **The table owner (or a superuser) can still disable the triggers** with `ALTER TABLE ... DISABLE TRIGGER`. In production, run the API with a database role that does not own the tables and has no `UPDATE`, `DELETE` or `TRUNCATE` privileges.
- **Events are delivered at least once and may arrive out of order.** Retries and several workers mean an older event can arrive after a newer one. Receivers should dedupe on the event id and not depend on order.
- **One webhook URL, and no tool for dead letters yet.** Events that give up stay in `outbox_events` with `failed_at` set; there is no endpoint or command to replay them.
- **Only postings made after the outbox migration have events.** Older transactions are not backfilled.

## Project layout

```
ledger/
  main.go                  entry point, wiring, outbox worker and graceful shutdown
  Dockerfile               multi-stage build of the API
  docker-compose.yml       database, migrations, API
  Makefile                 common commands
  sqlc.yaml                sqlc configuration
  .env.example             template for local configuration
  .gitignore               keeps .env and other private files out of Git
  .dockerignore            keeps .env, .git and build-irrelevant files out of the Docker image
  db/
    migrations/            schema, constraints, triggers, outbox
    queries/ledger.sql     SQL that sqlc turns into Go
  internal/
    api/                   Gin handlers and error mapping
    db/                    generated by sqlc (do not edit)
    ledger/                posting logic, reversals, validation, request hash, tests
    outbox/                outbox worker, retry logic, signed webhook publisher
  .github/workflows/ci.yml tests on every push
```

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.