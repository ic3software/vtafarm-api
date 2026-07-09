ALTER TABLE setup_sessions
    -- full_stack_with_vtc — the VTC component's subdomain/CF record follow the
    -- same pattern as mediator_subdomain/cf_record_mediator (000009)
    ADD COLUMN vtc_subdomain     VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN cf_record_vtc     VARCHAR(100),
    -- Inputs: vtc_name doubles as the VTA context id the community lives under.
    -- '' for modes without a VTC ('personal-vtc' historically; fixed in 000013,
    -- which also corrects this default on databases that already ran this file).
    ADD COLUMN vtc_name          TEXT         NOT NULL DEFAULT '',
    ADD COLUMN vtc_image         TEXT         NOT NULL DEFAULT '',
    -- Collected outputs
    ADD COLUMN vtc_setup_key_did TEXT         NOT NULL DEFAULT '',
    ADD COLUMN vtc_did           TEXT         NOT NULL DEFAULT '',
    ADD COLUMN vtc_admin_did     TEXT         NOT NULL DEFAULT '',
    -- Reveal-once install credentials, like mediator_admin_key/webvh_admin_key
    ADD COLUMN vtc_install_url   TEXT         NOT NULL DEFAULT '',
    ADD COLUMN vtc_claim_code    TEXT         NOT NULL DEFAULT '',
    ADD COLUMN vtc_install_used  BOOLEAN      NOT NULL DEFAULT FALSE;
