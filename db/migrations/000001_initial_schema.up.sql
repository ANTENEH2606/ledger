-- ============================================================
-- Chart of accounts
-- ============================================================
CREATE TABLE accounts (
    id         BIGSERIAL PRIMARY KEY,
    code       TEXT        NOT NULL UNIQUE,
    name       TEXT        NOT NULL,
    type       TEXT        NOT NULL
               CHECK (type IN ('asset', 'liability', 'equity', 'revenue', 'expense')),
    currency   TEXT        NOT NULL CHECK (char_length(currency) = 3),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================
-- Transactions: the whole event (journal entry header)
-- ============================================================
CREATE TABLE transactions (
    id                      BIGSERIAL PRIMARY KEY,
    -- Retrying the same request sends the same key; UNIQUE makes a duplicate impossible.
    idempotency_key         TEXT        NOT NULL UNIQUE,
    effective_date          DATE        NOT NULL,   -- when it happened (accounting date)
    description             TEXT        NOT NULL DEFAULT '',
    -- Corrections are new transactions that point back at the one they reverse.
    -- UNIQUE: a transaction can be reversed only once.
    reverses_transaction_id BIGINT      UNIQUE REFERENCES transactions (id),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now() -- when it was recorded
);

-- ============================================================
-- Entries: the lines inside a transaction
-- Signed amount in minor units: positive = DEBIT, negative = CREDIT.
-- ============================================================
CREATE TABLE entries (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id BIGINT      NOT NULL REFERENCES transactions (id),
    account_id     BIGINT      NOT NULL REFERENCES accounts (id),
    amount         BIGINT      NOT NULL CHECK (amount <> 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX entries_account_id_idx     ON entries (account_id);
CREATE INDEX entries_transaction_id_idx ON entries (transaction_id);
CREATE INDEX transactions_effective_date_idx ON transactions (effective_date);

-- ============================================================
-- Append-only: no UPDATE or DELETE on transactions or entries
-- (TRUNCATE is not blocked, so tests can still reset the database.)
-- ============================================================
CREATE FUNCTION forbid_modification() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the ledger is append-only. Post a reversing transaction instead.',
        TG_OP, TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER entries_append_only
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION forbid_modification();

CREATE TRIGGER transactions_append_only
    BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION forbid_modification();

-- ============================================================
-- Balance rule, enforced by the database at COMMIT time.
-- A CHECK constraint sees only one row, so a deferred constraint
-- trigger is used to look at all entries of the transaction together.
--   * at least 2 entries
--   * all accounts share one currency
--   * entries sum to zero (debits = credits)
-- ============================================================
CREATE FUNCTION assert_transaction_balanced() RETURNS trigger AS $$
DECLARE
    tx_id        BIGINT;
    total        BIGINT;
    n_entries    INT;
    n_currencies INT;
BEGIN
    IF TG_TABLE_NAME = 'transactions' THEN
        tx_id := NEW.id;
    ELSE
        tx_id := NEW.transaction_id;
    END IF;

    SELECT COALESCE(SUM(e.amount), 0)::BIGINT,
           COUNT(*),
           COUNT(DISTINCT a.currency)
      INTO total, n_entries, n_currencies
      FROM entries e
      JOIN accounts a ON a.id = e.account_id
     WHERE e.transaction_id = tx_id;

    IF n_entries < 2 THEN
        RAISE EXCEPTION 'transaction % must have at least 2 entries (has %)', tx_id, n_entries;
    END IF;
    IF n_currencies <> 1 THEN
        RAISE EXCEPTION 'transaction % mixes currencies', tx_id;
    END IF;
    IF total <> 0 THEN
        RAISE EXCEPTION 'transaction % is unbalanced: entries sum to %', tx_id, total;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Fires at commit for every inserted transaction (catches a transaction with no entries)...
CREATE CONSTRAINT TRIGGER transactions_must_balance
    AFTER INSERT ON transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();

-- ...and for every inserted entry (catches entries that do not add up).
CREATE CONSTRAINT TRIGGER entries_must_balance
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_transaction_balanced();