-- 000041 added the claim-reservation statuses but left rows written by the
-- 000039 flow as they were. The reservation code knows neither 'pending' nor
-- 'accepted' and treats an unknown latest row as still active, so creating or
-- refreshing a QR code on such a session answered 409 forever.
UPDATE mobile_connections
SET status = 'lapsed'
WHERE status = 'pending';

UPDATE mobile_connections
SET status = 'released',
    released_at = COALESCE(connected_at, provisioned_at, accepted_at, created_at)
WHERE status = 'accepted';
