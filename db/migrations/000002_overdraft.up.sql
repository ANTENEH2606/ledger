-- Accounts that must not go "overdrawn" (e.g. customer wallets).
-- Existing accounts keep the old behaviour (overdraft allowed).
ALTER TABLE accounts
    ADD COLUMN allow_overdraft BOOLEAN NOT NULL DEFAULT true;