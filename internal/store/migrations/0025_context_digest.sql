-- Binds a submission's review to the exact bytes that get built. The upload
-- records the sha256 of the stored context tarball; an admin approves naming
-- the digest they reviewed, and the approve CAS only wins while the row still
-- carries it. A re-upload while pending replaces both the blob and the digest,
-- so an approval issued against the old digest loses instead of building
-- content nobody saw. NULL marks a context uploaded before digests were kept
-- (or no upload yet); such a row cannot be approved until it is uploaded again.
ALTER TABLE image_submissions ADD COLUMN context_sha256 text;

-- The digest the build was allowed to consume. The context-fetch step refuses
-- any other bytes, so a context swapped after approval fails the build.
ALTER TABLE image_builds ADD COLUMN context_digest text;
