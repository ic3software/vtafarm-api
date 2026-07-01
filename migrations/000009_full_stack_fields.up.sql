ALTER TABLE setup_sessions
    -- Three subdomains instead of one (subdomain stays for vta_only back-compat).
    ADD COLUMN vta_subdomain      VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN mediator_subdomain VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN dids_subdomain     VARCHAR(100) NOT NULL DEFAULT '',
    -- One Cloudflare record id per host (cf_record_id stays for vta_only).
    ADD COLUMN cf_record_vta      VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN cf_record_mediator VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN cf_record_dids     VARCHAR(100) NOT NULL DEFAULT '',
    -- Per-component images (vta_image already exists).
    ADD COLUMN mediator_image     TEXT         NOT NULL DEFAULT '',
    ADD COLUMN dids_image         TEXT         NOT NULL DEFAULT '',
    -- Collected outputs (mediator_did/vta_did/admin_did already exist).
    ADD COLUMN mediator_admin_did TEXT         NOT NULL DEFAULT '',
    ADD COLUMN webvh_admin_did    TEXT         NOT NULL DEFAULT '',
    ADD COLUMN dids_daemon_did    TEXT         NOT NULL DEFAULT '',
    -- Admin private keys, returned to the user once; plaintext, like the PVCs.
    ADD COLUMN mediator_admin_key TEXT         NOT NULL DEFAULT '',
    ADD COLUMN webvh_admin_key    TEXT         NOT NULL DEFAULT '',
    ADD COLUMN dids_enroll_url    TEXT         NOT NULL DEFAULT '';
