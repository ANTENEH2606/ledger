-- Row-level triggers do not fire on TRUNCATE, so block it with statement-level triggers.
-- Reuses forbid_modification() from migration 000001.
CREATE TRIGGER entries_no_truncate
    BEFORE TRUNCATE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_modification();

CREATE TRIGGER transactions_no_truncate
    BEFORE TRUNCATE ON transactions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_modification();