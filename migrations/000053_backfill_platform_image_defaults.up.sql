-- Image defaults now follow every component of the platform stack, not just
-- its VTA (see model.SyncPlatformImageDefaults). A stack that was already
-- running when that changed never passes the sync points again until it is
-- upgraded, so copy its current images in once. Existing rows — the VTA
-- default synced by earlier upgrades, or a pin an admin saved — are kept.
INSERT INTO image_defaults (component, image)
SELECT c.component, c.image
FROM setup_sessions s
CROSS JOIN LATERAL (VALUES
    ('vta', s.vta_image),
    ('mediator', s.mediator_image),
    ('dids', s.dids_image),
    ('vtc', s.vtc_image),
    ('trust-registry', CASE WHEN s.trust_registry_state = 'active' THEN s.trust_registry_image ELSE '' END)
) AS c (component, image)
WHERE s.domain_type = 'platform'
  AND s.mode = 'full_stack'
  AND s.status = 'running'
  AND c.image <> ''
ON CONFLICT (component) DO NOTHING;
