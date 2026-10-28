DROP TABLE pool_batch_additions;

DROP INDEX mobile_connections_public_access_digest_unique;
ALTER TABLE mobile_connections
    DROP CONSTRAINT mobile_connections_public_access_digest_check,
    DROP COLUMN public_access_digest;

DROP INDEX pool_batches_public_signup_enabled_idx;
ALTER TABLE pool_batches
    DROP CONSTRAINT pool_batches_public_signup_enabled_check,
    DROP CONSTRAINT pool_batches_purpose_check,
    DROP COLUMN public_signup_enabled_at,
    DROP COLUMN purpose;
