
-- ============================================================
-- Accounts
-- ============================================================
 
-- name: CreateAccount :one
INSERT INTO accounts (code, name, type, currency)
VALUES ($1, $2, $3, $4)
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
INSERT INTO transactions (idempotency_key, effective_date, description, reverses_transaction_id)
VALUES ($1, $2, $3, $4)
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
SELECT
    a.id,
    a.code,
    a.name,
    a.type,
    a.currency,
    COALESCE(SUM(e.amount) FILTER (WHERE e.amount > 0), 0)::bigint  AS total_debits,
    COALESCE(-SUM(e.amount) FILTER (WHERE e.amount < 0), 0)::bigint AS total_credits,
    COALESCE(SUM(e.amount), 0)::bigint                              AS balance
FROM accounts a
LEFT JOIN entries e ON e.account_id = a.id
LEFT JOIN transactions t ON t.id = e.transaction_id
                        AND t.effective_date <= @as_of::date
WHERE e.id IS NULL OR t.id IS NOT NULL
GROUP BY a.id, a.code, a.name, a.type, a.currency
ORDER BY a.code;
 
