
-- ============================================================
-- Accounts
-- ============================================================
 
-- name: CreateAccount :one
INSERT INTO accounts (code, name, type, currency, allow_overdraft)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;
 
-- name: GetAccount :one
SELECT * FROM accounts WHERE id = $1;
 
-- name: GetAccountByCode :one
SELECT * FROM accounts WHERE code = $1;
 
-- name: ListAccounts :many
SELECT * FROM accounts ORDER BY code;
 
-- name: LockAccountsForUpdate :many
-- Locks the given accounts in a fixed order (by id) so that concurrent
-- transfers cannot deadlock. Call inside the posting transaction.
SELECT id, currency, type, allow_overdraft FROM accounts
WHERE id = ANY(@ids::bigint[])
ORDER BY id
FOR UPDATE;
 
-- name: SetAllowOverdraft :exec
UPDATE accounts SET allow_overdraft = $2 WHERE id = $1;
 
-- name: GetAccountBalance :one
-- Signed balance: positive = net debit, negative = net credit.
SELECT COALESCE(SUM(amount), 0)::bigint AS balance
FROM entries
WHERE account_id = $1;
 
-- ============================================================
-- Transactions and entries
-- ============================================================
 
-- name: GetTransactionByIdempotencyKey :one
SELECT * FROM transactions WHERE idempotency_key = $1;
 
-- name: CreateTransaction :one
-- ON CONFLICT DO NOTHING returns no row when the key already exists
-- (a retried request); the caller then loads the original transaction.
INSERT INTO transactions (idempotency_key, effective_date, description, reverses_transaction_id,request_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING *;
 
-- name: CreateEntry :one
INSERT INTO entries (transaction_id, account_id, amount)
VALUES ($1, $2, $3)
RETURNING *;
 
-- name: GetTransaction :one
SELECT * FROM transactions WHERE id = $1;
 
-- name: ListEntriesByTransaction :many
SELECT * FROM entries WHERE transaction_id = $1 ORDER BY id;
 
-- ============================================================
-- Reports
-- ============================================================
 
-- name: GetAccountStatement :many
-- Every entry on an account with a running balance (oldest first).
SELECT
    e.id,
    t.effective_date,
    t.description,
    e.amount,
    (SUM(e.amount) OVER (ORDER BY t.effective_date, e.id))::bigint AS running_balance
FROM entries e
JOIN transactions t ON t.id = e.transaction_id
WHERE e.account_id = $1
ORDER BY t.effective_date, e.id;

-- name: GetAccountStatementPage :many
-- One page of the statement, oldest first. The running balance covers the
-- whole history; the cursor (date, id) only chooses which rows to return.
SELECT s.id, s.effective_date, s.description, s.amount, s.running_balance
FROM (
    SELECT
        e.id,
        t.effective_date,
        t.description,
        e.amount,
        (SUM(e.amount) OVER (ORDER BY t.effective_date, e.id))::bigint AS running_balance
    FROM entries e
    JOIN transactions t ON t.id = e.transaction_id
    WHERE e.account_id = @account_id
) s
WHERE NOT @use_cursor::bool
   OR (s.effective_date, s.id) > (@after_date::date, @after_id::bigint)
ORDER BY s.effective_date, s.id
LIMIT @page_size::int;
 
-- name: GetAccountBalanceAsOf :one
-- Signed balance: positive = net debit, negative = net credit.
SELECT COALESCE(SUM(e.amount), 0)::bigint AS balance
FROM entries e
JOIN transactions t ON t.id = e.transaction_id
WHERE e.account_id = @account_id
  AND t.effective_date <= @as_of::date;
 
-- name: GetTrialBalance :many
-- Debit and credit totals per account as of a date. Across all accounts
-- (per currency) the total debits must equal the total credits.
SELECT    a.id, a.code, a.name, a.type, a.currency,
    COALESCE(SUM(x.amount) FILTER (WHERE x.amount > 0), 0)::bigint  AS total_debits,
    COALESCE(-SUM(x.amount) FILTER (WHERE x.amount < 0), 0)::bigint AS total_credits,
    COALESCE(SUM(x.amount), 0)::bigint                              AS balance
FROM accounts a
LEFT JOIN (
    SELECT e.account_id, e.amount
    FROM entries e
    JOIN transactions t ON t.id = e.transaction_id
    WHERE t.effective_date <= @as_of::date
) x ON x.account_id = a.id
GROUP BY a.id, a.code, a.name, a.type, a.currency
ORDER BY a.code;
 
-- name: HasEntriesAfter :one
-- True if the account already has an entry dated after @on_date.
SELECT EXISTS (
    SELECT 1
    FROM entries e
    JOIN transactions t ON t.id = e.transaction_id
    WHERE e.account_id = @account_id
      AND t.effective_date > @on_date::date
) AS found;

-- name: CreateOutboxEvent :one
INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListOutboxEventsByAggregate :many
SELECT * FROM outbox_events
WHERE aggregate_type = $1 AND aggregate_id = $2
ORDER BY id;

-- name: ClaimOutboxBatch :many
-- Unsent events that are due, oldest first. Rows locked by another worker are skipped.
SELECT * FROM outbox_events
WHERE published_at IS NULL
  AND failed_at IS NULL
  AND next_attempt_at <= now()
ORDER BY id
LIMIT @batch_size::int
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxPublished :exec
UPDATE outbox_events
SET published_at = now(), attempts = attempts + 1, last_error = NULL
WHERE id = $1;

-- name: MarkOutboxFailed :exec
-- Count the failure, schedule the retry, and give up after max_attempts.
UPDATE outbox_events
SET attempts        = attempts + 1,
    last_error      = @last_error,
    next_attempt_at = now() + make_interval(secs => @retry_after_seconds::int),
    failed_at       = CASE WHEN attempts + 1 >= @max_attempts::int THEN now() ELSE failed_at END
WHERE id = @id;

