-- User-submitted modpack approval lane (a user-directed extension over the §16
-- build subsystem; see internal/submit for provenance). This is the UNTRUSTED-
-- origin counterpart to the admin build path (POST /images/build): an ordinary
-- user may upload a modpack but cannot start a build directly. Each upload lands
-- here as pending_review; an admin must approve it before anything is built, and
-- the approved submission then routes through the SAME Trivy-gated Kaniko build
-- as an admin build (build subsystem §16). Approval is a human gate layered in
-- FRONT of the automatic scan, never instead of it — a CRITICAL CVE still fails
-- the build and nothing is admitted even after a human approved.
--
-- Source of truth (spec §1): this row is the Postgres BUSINESS authority for the
-- approval (verdict + reviewer); the build EXECUTION lives in image_builds,
-- linked by build_id once Builder.Submit succeeds. The approval never copies the
-- build's authoritative fields.
--
-- Trust note: the platform derives BOTH the push target (image_ref) and the
-- build context (context_ref) from the submission id — neither is free-form user
-- input — so an untrusted submitter can never point the build at an arbitrary
-- source or collide with the platform image namespace. There is deliberately no
-- `origin` column: image_submissions is ONLY the user-upload lane (the platform
-- uses the direct build path), and submitted_by already records the origin.

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
