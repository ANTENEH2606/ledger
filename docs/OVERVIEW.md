# Ledger: overview

A backend service that records money movements the way accounting does: every transaction has entries that must sum to zero, history is never edited, and mistakes are fixed by posting a reversal. I built it to learn how to make financial code stay correct when requests are retried and arrive at the same time. I studied accounting and finance, and this project is where that background meets backend engineering.

Built with Go, PostgreSQL, pgx and sqlc. For setup, the API and troubleshooting, see the [README](../README.md).

## What it guarantees, and how

| Guarantee | How |
| --- | --- |
| **Every transaction balances** | A deferred constraint trigger checks at commit that entries sum to zero, in one currency, with at least two entries. Raw SQL cannot bypass it. |
| **History cannot be rewritten** | Triggers block `UPDATE`, `DELETE` and `TRUNCATE` on the ledger tables. |
| **Retries never double-post** | Each posting carries an idempotency key. A retry returns the original (`200`). The same key with different content is rejected (`409`). |
| **No lost updates under concurrency** | Posting locks the accounts involved, in id order, so rules that read a balance cannot race. In a test, 30 simultaneous withdrawals hit a wallet holding enough for 10: exactly 10 succeed. Without the locks, 24 got through. |
| **Events are never lost or invented** | A transactional outbox writes each event in the same database transaction as the posting. A worker delivers signed webhooks at least once, with backoff and a dead-letter state. |
| **A mistake is corrected exactly once** | A reversal posts the flipped entries and links to the original. A unique constraint means the database itself refuses a second reversal. |

## Decisions, and why

- **Signed `BIGINT` amounts in minor units.** "Debits equal credits" becomes `SUM(amount) = 0`, which the database can check directly. No floats, so no rounding errors.
- **Rules in the database, not only in Go.** Application code has bugs and other writers can exist. Triggers and constraints hold for every client.
- **Locks only where a rule reads a balance.** Plain inserts into append-only history are safe to run concurrently. Locks are needed for rules such as "no overdraft", and always taken in id order to rule out deadlocks.
- **Reversals reuse the normal posting path.** They obey the same balance, overdraft, idempotency and outbox rules with no special cases. The database guarantees "at most once"; the code only turns that failure into a clean `409`.
- **A versioned request fingerprint for idempotency.** When I added reversals I kept the hash of normal postings unchanged so stored hashes stay valid, and a golden-value test fails if it ever drifts.

## How I know it works

Tests run against a real PostgreSQL on every push in CI, including concurrency tests under Go's race detector and tests that attack the triggers with raw SQL.

For each rule I also did a sabotage check: break the code on purpose and confirm that exactly the right test fails. A test I have never seen fail is not evidence.

## What it does not do yet

- No authentication, so idempotency keys are global.
- No foreign exchange: one currency per transaction.
- The statement's running balance is recomputed on every page, which is slow for very large accounts.
- Events can arrive out of order, so receivers must dedupe on the event id.
- A reversal cannot itself be reversed.

The full list is in the README's [Known limits](../README.md#known-limits).