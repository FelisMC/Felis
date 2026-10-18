-- Administrator approval permits building but does not bypass the mandatory scan gate.

CREATE TYPE submission_status AS ENUM ('pending_review','approved','rejected');

CREATE TABLE image_submissions (
  id            text PRIMARY KEY,             -- lowercase, namespaces the derived image/context refs
  submitted_by  text NOT NULL,                -- uploading user's id (untrusted origin)
  display_name  text NOT NULL,                -- human-friendly label for the modpack
  context_ref   text NOT NULL,                -- DERIVED pinned build context (not user-supplied)
  status        submission_status NOT NULL DEFAULT 'pending_review',
  image_ref     text,                         -- DERIVED {registry}/user-uploads/{id}:latest, set at approve
  build_id      text,                         -- image_builds.id, set only after Builder.Submit succeeds
  reviewed_by   text,                         -- admin who approved/rejected
  reject_reason text,                         -- set on rejection
  created_at    timestamptz NOT NULL DEFAULT now(),
  reviewed_at   timestamptz
);

-- The admin review queue scans by status (pending first); the per-user index
-- serves the "my submissions" list.
CREATE INDEX image_submissions_status_idx       ON image_submissions (status, created_at);
CREATE INDEX image_submissions_submitted_by_idx ON image_submissions (submitted_by, created_at DESC);
