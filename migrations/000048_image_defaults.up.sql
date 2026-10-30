-- Admin-pinned default image per component. The user-facing create form
-- preselects the pinned image; without a row it follows the "latest" tag.
CREATE TABLE image_defaults (
    component TEXT PRIMARY KEY CHECK (component IN ('vta', 'mediator', 'dids', 'vtc', 'trust-registry')),
    image TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
