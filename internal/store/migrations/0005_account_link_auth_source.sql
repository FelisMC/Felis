-- Default existing account links to mojang when no source was recorded.
CREATE TYPE link_auth_source AS ENUM ('mojang','thirdparty');

ALTER TABLE account_link_codes
  ADD COLUMN auth_source link_auth_source NOT NULL DEFAULT 'mojang';
ALTER TABLE account_links
  ADD COLUMN auth_source link_auth_source NOT NULL DEFAULT 'mojang';
