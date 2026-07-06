ALTER TABLE setup_sessions
    DROP COLUMN IF EXISTS vtc_subdomain,
    DROP COLUMN IF EXISTS cf_record_vtc,
    DROP COLUMN IF EXISTS vtc_name,
    DROP COLUMN IF EXISTS vtc_image,
    DROP COLUMN IF EXISTS vtc_setup_key_did,
    DROP COLUMN IF EXISTS vtc_did,
    DROP COLUMN IF EXISTS vtc_admin_did,
    DROP COLUMN IF EXISTS vtc_install_url,
    DROP COLUMN IF EXISTS vtc_claim_code,
    DROP COLUMN IF EXISTS vtc_install_used;
