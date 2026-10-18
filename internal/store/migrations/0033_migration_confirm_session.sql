-- Migration step-up counts only for the session that gave it, for a few minutes (migrateConfirmWindow).
-- confirm_session holds that session's token hash.
ALTER TABLE account_migrations ADD COLUMN confirm_session text;
