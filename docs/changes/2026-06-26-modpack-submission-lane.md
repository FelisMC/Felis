# Modpack submission lane: build/approval pipeline + storage backends (ledger backfill)

- **Type:** feature — retroactive ledger entry
- **Date:** 2026-06-26 – 2026-07-02
- **Area:** `internal/submit` (build/approval pipeline, storage backends), `internal/api` (submission endpoints)
- **Commits:**
  - `d39605e` feat(submit): user modpack build + approval pipeline — an uploaded modpack stays `pending_review` and is never built until an admin approves; approval is a single-winner compare-and-swap handing off to the image-build Job, keeping the mandatory vulnerability scan in front of any push
  - `598f3d3` feat(submit): local + S3 backends for modpack upload contexts, installer-selectable
- **Tasks:** #24 (§8 user-submitted modpack approval lane)

## What it did

Built the user-directed extension over the image-build subsystem: a player uploads a
modpack context, it sits in `pending_review`, and an admin's approval is the single-winner
gate that hands off to the build Job — with the vulnerability scan always ahead of any
registry push. `598f3d3` makes the upload-context store pluggable (local filesystem or S3),
selectable at install time.

## Why

Untrusted user content must never build or push unreviewed, and the compare-and-swap
approval guarantees exactly one build per submission even under a double-click or retry.
The storage-backend choice lets a single-node demo use local disk while a real deployment
uses S3, without a code change.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The approval
> compare-and-swap and endpoints were unit-tested at their commits; the S3 path is
> integration-configurable. The panel-side submission/approval UI is the collaborator's
> frontend work and is tracked only by its INDEX rows. Not independently re-verified for
> this doc; current tree green at `9911b8c`.
