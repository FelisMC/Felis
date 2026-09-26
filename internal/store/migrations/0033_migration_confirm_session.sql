-- The migration step-up counts for the session that gave it, and only for a few
-- minutes (migrateConfirmWindow in handlers_account_migrate.go). confirm_session is
-- that session's token hash ('' for a caller the proxy authenticates on every
-- request). Another signed-in session of the source account, or the same one later,
-- must prove a factor again before it can issue the code that hands the servers away.
ALTER TABLE account_migrations ADD COLUMN confirm_session text;
