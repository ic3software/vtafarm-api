-- The did_hosting_did backfill is deliberately not reversed. Down migrations
-- restore the schema, not a snapshot of the data, and the backfilled value is
-- correct independently of this feature: it records which daemon's DID a
-- vta_only session's control URL actually answers with. Blanking it would
-- disarm Factory.For's audience check on rows that predate the rollback for no
-- gain, and there is nothing to distinguish a backfilled value from one written
-- since.
DROP INDEX IF EXISTS setup_sessions_provider_idx;

ALTER TABLE setup_sessions
    DROP COLUMN IF EXISTS provider_session_id,
    DROP COLUMN IF EXISTS connection_source,
    DROP COLUMN IF EXISTS share_code;
