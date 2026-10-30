![CI](https://github.com/ANTENEH2606/ledger/actions/workflows/ci.yml/badge.svg)

# Ledger

A double-entry ledger service written in Go, backed by PostgreSQL. It is built to stay correct under retries and concurrent requests: money is never lost, never double-posted, and every transaction balances. The important rules are enforced by the database itself, not only by application code.

> **Status:** core ledger complete. Atomic posting with idempotency, concurrency-safe overdraft protection, an HTTP API, and reports (statement, balance as of a date, trial balance). Tested on every push by CI, including concurrency tests under Go's race detector. No authentication yet (see [Known limits](#known-limits)).

## What it guarantees

| Guarantee | How it is enforced |
| --- | --- |
| **Every transaction balances** | A deferred constraint trigger checks at `COMMIT` that a transaction has at least two entries, in one currency, summing to zero. An unbalanced transaction cannot be committed. |
| **History is never rewritten** | `UPDATE` and `DELETE` on `entries` and `transactions` are blocked by triggers. Mistakes are corrected with reversing transactions, as in real accounting. |
| **Retries never double-post** | `idempotency_key` is `UNIQUE`; the insert uses `ON CONFLICT DO NOTHING`, and a retry returns the original transaction. |
| **A reused key with different content is rejected** | A SHA-256 fingerprint of the request (entries and effective date) is stored with the key. Same key and different content returns `409`. |
| **No lost updates under concurrency** | Posting locks every account involved with `SELECT ... FOR UPDATE`, in id order, so transfers in opposite directions cannot deadlock. Deadlocks and serialization failures are retried. |
| **No overdraft where forbidden** | Accounts with `allow_overdraft = false` are checked against a balance read *while the account is locked*, so simultaneous withdrawals cannot all pass the check. |
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

Run the same transaction command again: you get the same transaction back, and nothing is posted twice.

## API

| Method and path | Purpose |
| --- | --- |
| `POST /accounts` | Create an account (`code`, `name`, `type`, `currency`) |
| `POST /transactions` | Post a transaction. Requires the `Idempotency-Key` header. |
| `GET /accounts/:id/statement` | Entries on an account with a running balance |
| `GET /accounts/:id/balance?as_of=YYYY-MM-DD` | Balance at the end of a date (default: today) |
| `GET /trial-balance?as_of=YYYY-MM-DD` | Debits, credits and balance per account, with totals per currency |
| `GET /health` | Liveness and database check |

All amounts are integers in minor units. In a transaction, positive is a debit and negative is a credit. The effective date is supplied by the client.

| Status | Meaning |
| --- | --- |
| `200` | ok : The request worked. Used when you read something (GET): a statement, a balance, the trial balance, the health check |
| `201` | Created : The request worked and something new was created. Your API returns it for POST /accounts and POST /transactions |
| `400` | Malformed request: missing key, fewer than two entries, a zero amount, unbalanced, bad date |
| `404` | Account not found |
| `409` | Idempotency key reused with different content, or duplicate account code |
| `422` | Valid request that breaks a business rule: unknown account, mixed currencies, insufficient funds |
| `500` | Server Error : Something went wrong on the server (never shown with details) |

## Design decisions

- **Signed amounts.** One column instead of separate debit and credit columns. "Debits equal credits" becomes `SUM(amount) = 0`, which the database can check directly.
- **Rules in the database.** Application code can have bugs and other writers can exist. Triggers and constraints make the invariants hold for every client.
- **Locks only where a rule reads a balance.** Because balances are derived from append-only entries, plain concurrent inserts are safe. Account locks are needed for rules such as "no overdraft", where a check-then-act race would otherwise be possible. Accounts are always locked in id order to rule out deadlocks.
- **A versioned request fingerprint.** The hash covers only what the client sent (sorted entries and the effective date) with a `ledger/request/v1` prefix. It leaves out the description and anything server-generated, so a retry always produces the same hash.
- **Corrections by reversal.** The schema supports a `reverses_transaction_id` link (unique). There is no reversal endpoint yet.

## Development

Requires Go (see `go.mod`), Docker, and the [`migrate`](https://github.com/golang-migrate/migrate) CLI.

```bash
make up                 # start the full stack in Docker
make down               # stop it (data stays in the ledger_data volume)

make migrate-up         # apply migrations to the main database
make psql               # open psql on the main database
make run                # run the API on the host

make test-db-create     # once: create the separate ledger_test database
make test-db-migrate    # apply migrations to the test database
make test-db            # run all tests against ledger_test
```

Tests run against a separate `ledger_test` database so they never touch real data. Database tests are skipped when `DATABASE_URL` is not set, so check for `PASS` and not `SKIP` when running them with `-v`.

After changing a migration or `db/queries/ledger.sql`, run `sqlc generate`.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `password authentication failed` | Postgres only reads the password when it first creates its data volume. If you changed `PG_PASSWORD` later, either change the database password with `ALTER USER`, or remove the volume (`docker volume rm ledger_data`, **which deletes all data**) and start again. |
| `port is already allocated` | Another program uses `5433` or `8080`. Stop it, or change the ports in `docker-compose.yml`. |
| `container name "/ledger" is already in use` | An older container with that name exists. Remove it with `docker rm ledger` (without `-v`) and run `docker compose up -d` again. |
| `DATABASE_URL is not set` | Run the command through `make`, which reads `.env`, or export the variables yourself. |

## Known limits

- **No authentication or authorization.** The API listens on `127.0.0.1` only. Idempotency keys are global; a real deployment should scope them per authenticated client.
- **Hot accounts serialize.** Postings that touch the same account queue on its lock.
- **No reversal endpoint yet** (the schema supports it), and no pagination on statements or the trial balance.
- **A replayed request returns `201`**, the same as the first one.
- **One currency per transaction.** There is no foreign-exchange support.

## Project layout

```
ledger/
  main.go                  entry point and wiring
  Dockerfile               multi-stage build of the API
  docker-compose.yml       database, migrations, API
  Makefile                 common commands
  sqlc.yaml                sqlc configuration
  .env.example             template for local configuration
  .gitignore               keeps .env and other private files out of Git
  .dockerignore            keeps .env and .git out of the Docker image
  db/
    migrations/            schema, constraints, triggers
    queries/ledger.sql     SQL that sqlc turns into Go
  internal/
    api/                   Gin handlers and error mapping
    db/                    generated by sqlc (do not edit)
    ledger/                posting logic, validation, request hash, tests
  .github/workflows/ci.yml tests on every push
```

## License

Not yet chosen.