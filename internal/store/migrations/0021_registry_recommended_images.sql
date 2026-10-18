-- Move only unchanged seed references; preserve existing rows on reference collisions.
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
