![CI](https://github.com/ANTENEH2606/ledger/actions/workflows/ci.yml/badge.svg)

# Ledger

A double-entry ledger service written in Go, backed by PostgreSQL. The goal is a ledger that stays correct under retries and concurrent requests: money is never lost, never double-posted, and every transaction balances.

> **Status:** early development. Milestone 1 (project setup and database connection) is complete.

## Tech stack

- **Go** for the service
- **PostgreSQL 16** (Docker) for storage
- **pgx** as the Postgres driver
- **sqlc** for type-safe queries (planned)
- **Gin** for the HTTP API (planned)

## Design principles

- **Double-entry:** every transaction consists of entries whose debits equal credits.
- **Append-only:** entries are never updated or deleted. Mistakes are fixed with reversing transactions, as in real accounting.
- **Integer money:** amounts are stored as `BIGINT` in minor units (cents, or santim for ETB). Never floats.
- **Derived balances:** balances are computed from entries, not stored and overwritten.
- **Idempotency:** retrying the same request never posts a transfer twice.
- **Concurrency-safe:** simultaneous transfers on the same account cannot corrupt a balance.

## Getting started

### Prerequisites

- [Go](https://go.dev/dl/) 1.22 or newer
- [Docker](https://docs.docker.com/get-docker/)
- Git

### 1. Clone the repository

```bash
git clone https://github.com/ANTENEH2606/ledger.git
cd ledger
```

### 2. Configure the environment

Copy the example file and choose your own password. Use the same password in both places inside `.env`:

```bash
cp .env.example .env
```

`.env` format:

```
POSTGRES_USER=ledger
POSTGRES_PASSWORD=<your-password>
POSTGRES_DB=ledger
DATABASE_URL=postgres://ledger:<your-password>@localhost:5433/ledger
```

`.env` is private: it is listed in `.gitignore` and must never be committed. Only `.env.example` (with fake values) is shared.

### 3. Start PostgreSQL

The database reads its credentials from `.env`, so no password appears in any command:

```bash
docker run --name ledger \
  --env-file .env \
  -p 127.0.0.1:5433:5432 \
  -d postgres:16
```

The `127.0.0.1:` prefix keeps the database reachable only from your own machine.

`.env` is listed in `.gitignore` and must never be committed.

### 4. Run

Load the variables into your shell, then run the app:

```bash
export $(grep -v '^#' .env | xargs)
go run .
```

If everything is connected, it prints `1`.

## Troubleshooting

| Error | Cause and fix |
| --- | --- |
| `PG_URL is not set` | The variable isn't loaded in this terminal. Run the `export` command from step 4. |
| `password authentication failed` | The password in `.env` doesn't match the container. Check with `docker inspect ledger` or recreate the container. |
| `connection refused` | Docker isn't running or Postgres is still starting. Wait a few seconds and retry. |
| `package ledger is not a main package` | Run `go run .` from the folder that contains `main.go`. |

## Roadmap

- [x] **Milestone 1:** project setup, Docker Postgres, pgx connection
- [ ] **Milestone 2:** schema (accounts, transactions, entries, idempotency keys) with migrations
- [ ] **Milestone 3:** `PostTransaction` with sqlc and pgx, in a single DB transaction
- [ ] **Milestone 4:** correctness tests (unbalanced rejected, concurrent transfers, idempotent retries)
- [ ] **Milestone 5:** HTTP API (create account, post transaction, statement)
- [ ] **Milestone 6:** reports (trial balance, balance as of a date)
- [ ] **Milestone 7:** docker-compose, CI with GitHub Actions, design write-up

## Project layout

```
ledger/
  main.go          entry point
  go.mod
  go.sum
  .env.example     template for local configuration
  .gitignore
  README.md
```

## License

Not yet chosen.