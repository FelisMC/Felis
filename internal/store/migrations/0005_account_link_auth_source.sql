-- Player onboarding (spec §10, dual-Yggdrasil): record HOW the in-game identity
-- authenticated when a link code was minted — 'mojang' (Mojang/official Yggdrasil,
-- the priority source) or 'thirdparty' (a configured alternate Yggdrasil). The
-- value is known only in-game at the moment online-mode auth established the UUID,
-- so it is captured on the code at mint time and copied onto the durable link at
-- verify. The web verify side never sees the authentication and cannot originate
-- it — the same constraint that puts mc_uuid (not user_id) on a code.
--
-- DEFAULT 'mojang' backfills any code/link rows that predate this column and gives
-- a Mojang-priority default for a mint that omits the field; the mint path supplies
-- it explicitly going forward.
CREATE TYPE link_auth_source AS ENUM ('mojang','thirdparty');

ALTER TABLE account_link_codes
  ADD COLUMN auth_source link_auth_source NOT NULL DEFAULT 'mojang';
ALTER TABLE account_links
  ADD COLUMN auth_source link_auth_source NOT NULL DEFAULT 'mojang';
