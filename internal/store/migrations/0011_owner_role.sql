-- Owner role: a platform-level identity above admin. Only an owner may manage
-- users (create / edit / disable / delete / reset password / manage quotas and
-- sessions). The role is added at the end of the enum so it sorts after 'user'
-- and 'admin' — safe for existing data and type comparisons.
--
-- The Owner account is minted by `felis breakGlass` at first-run bootstrap;
-- additional Operators created later are role='admin'. The Owner is the one
-- identity that can never be demoted or deleted through the panel — only
-- another owner (the break-glass console) may reset a lost owner.
--
-- UPGRADE NOTE: after applying this migration, your existing first admin
-- account is still role='admin'. Re-provision it via `felis breakGlass` (the
-- Owner path) to promote it to role='owner'. That path is idempotent — it
-- preserves the existing user id and resets the credential, now with the owner
-- role. Alternatively, run directly:
--   UPDATE users SET role = 'owner' WHERE id = '<your-owner-user-id>';

ALTER TYPE user_role ADD VALUE 'owner';
