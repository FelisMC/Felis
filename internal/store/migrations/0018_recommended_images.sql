-- Custom FELIS_LOBBY_IMAGE installs must reseed this row manually because POST /images rejects bare local tags.

-- ON CONFLICT keeps an admin's disable or re-point of the row across re-runs.
INSERT INTO image_whitelist (image_ref, source, added_by, enabled)
VALUES ('felis-lobby:demo', 'recommended', 'felis-platform', true)
ON CONFLICT (image_ref) DO NOTHING;
