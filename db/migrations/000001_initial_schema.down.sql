DROP TRIGGER IF EXISTS entries_must_balance ON entries;
DROP TRIGGER IF EXISTS transactions_must_balance ON transactions;
DROP FUNCTION IF EXISTS assert_transaction_balanced();

DROP TRIGGER IF EXISTS entries_append_only ON entries;
DROP TRIGGER IF EXISTS transactions_append_only ON transactions;
DROP FUNCTION IF EXISTS forbid_modification();

DROP TABLE IF EXISTS entries;
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;