DROP TABLE trust_task_executions;

DROP INDEX mobile_connections_active_session;
DROP INDEX mobile_connections_claim_handle_unique;

UPDATE mobile_connections
SET connected_at = COALESCE(connected_at, released_at)
WHERE status = 'released';

UPDATE mobile_connections
SET status = CASE status
    WHEN 'open' THEN 'pending'
    WHEN 'claimed' THEN 'cancelled'
    WHEN 'confirmed' THEN 'accepted'
    WHEN 'provisioning' THEN 'accepted'
    WHEN 'released' THEN 'accepted'
    WHEN 'rejected' THEN 'cancelled'
    WHEN 'lapsed' THEN 'expired'
    ELSE status
END;

ALTER TABLE mobile_connections
    DROP CONSTRAINT mobile_connections_redeem_poll_check,
    DROP CONSTRAINT mobile_connections_claim_timestamps_check,
    DROP CONSTRAINT mobile_connections_claimant_digest_check,
    DROP CONSTRAINT mobile_connections_claim_handle_check,
    DROP CONSTRAINT mobile_connections_status_check,
    ADD CONSTRAINT mobile_connections_status_check CHECK (
        status IN ('pending', 'accepted', 'cancelled', 'expired', 'failed')
    ),
    DROP COLUMN released_at,
    DROP COLUMN redeem_poll_until,
    DROP COLUMN redeem_poll_id,
    DROP COLUMN confirmed_at,
    DROP COLUMN claimed_at,
    DROP COLUMN confirm_by,
    DROP COLUMN claimant_digest,
    DROP COLUMN claim_handle;

CREATE UNIQUE INDEX mobile_connections_pending_session
    ON mobile_connections(session_id)
    WHERE status = 'pending';
CREATE UNIQUE INDEX mobile_connections_active_session
    ON mobile_connections(session_id)
    WHERE status = 'accepted' AND connected_at IS NULL;
