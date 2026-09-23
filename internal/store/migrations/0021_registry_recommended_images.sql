-- Re-point the platform-seeded recommended images at the internal registry.
--
-- 0018/0019 seeded 'felis-lobby:demo' and 'felis-paper:demo' — the bare local
-- containerd tags the bootstrap of that day imported. The installer now builds
-- every image under registry.<ns>.svc:5000/felis/... and mirrors it into the
-- in-cluster registry, which is what lets kubelet re-pull an image the image GC
-- collected (drilled: disk pressure → game images collected →
-- ImagePullBackOff with no pull source). A bare local tag has no pull source at
-- all once its containerd copy is collected, so a user server created from a
-- recommended row would strand the same way. Re-point the seeds at the refs the
-- installer now builds — these MUST stay identical to deploy/bootstrap.sh's
-- FELIS_LOBBY_IMAGE / FELIS_PAPER_IMAGE defaults.
--
-- Only source='recommended' rows are touched, and only while the ref still IS
-- the old seed: a built/external row, or a recommended row an admin re-pointed
-- by hand, is theirs to keep. enabled is preserved either way — a disabled seed
-- stays disabled, just under its durable name.
--
-- Collision handling: image_ref is the primary key, so an UPDATE would abort if
-- the new ref already exists (e.g. an admin added it by hand). Keep whichever
-- row exists and drop the stale old one — never a duplicate, never an aborted
-- migration.
UPDATE image_whitelist
SET image_ref = 'registry.felis.svc:5000/felis/lobby:demo'
WHERE image_ref = 'felis-lobby:demo'
  AND source = 'recommended'
  AND NOT EXISTS (SELECT 1 FROM image_whitelist WHERE image_ref = 'registry.felis.svc:5000/felis/lobby:demo');

DELETE FROM image_whitelist
WHERE image_ref = 'felis-lobby:demo'
  AND source = 'recommended'
  AND EXISTS (SELECT 1 FROM image_whitelist WHERE image_ref = 'registry.felis.svc:5000/felis/lobby:demo');

UPDATE image_whitelist
SET image_ref = 'registry.felis.svc:5000/felis/paper:demo'
WHERE image_ref = 'felis-paper:demo'
  AND source = 'recommended'
  AND NOT EXISTS (SELECT 1 FROM image_whitelist WHERE image_ref = 'registry.felis.svc:5000/felis/paper:demo');

DELETE FROM image_whitelist
WHERE image_ref = 'felis-paper:demo'
  AND source = 'recommended'
  AND EXISTS (SELECT 1 FROM image_whitelist WHERE image_ref = 'registry.felis.svc:5000/felis/paper:demo');
