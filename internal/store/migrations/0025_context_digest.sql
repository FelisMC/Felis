-- Bind approval to the reviewed context digest; legacy NULL digests require reupload.
ALTER TABLE image_submissions ADD COLUMN context_sha256 text;

-- The digest the build was allowed to consume. The context-fetch step refuses
-- any other bytes, so a context swapped after approval fails the build.
ALTER TABLE image_builds ADD COLUMN context_digest text;
