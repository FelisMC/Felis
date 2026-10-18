-- Keep existing image choices on conflict; custom FELIS_PAPER_IMAGE installs must replace the default seed.
INSERT INTO image_whitelist (image_ref, source, added_by, enabled)
VALUES ('felis-paper:demo', 'recommended', 'felis-platform', true)
ON CONFLICT (image_ref) DO NOTHING;
