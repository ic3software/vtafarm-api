DELETE FROM pool_batches AS batch
WHERE NOT EXISTS (
    SELECT 1 FROM pool_sessions AS membership WHERE membership.batch_id = batch.id
);

ALTER TABLE pool_batches ADD COLUMN vta_image text;

UPDATE pool_batches AS batch
SET vta_image = source.vta_image
FROM (
    SELECT DISTINCT ON (membership.batch_id)
        membership.batch_id,
        session.vta_image
    FROM pool_sessions AS membership
    JOIN setup_sessions AS session ON session.id = membership.session_id
    ORDER BY membership.batch_id, session.id
) AS source
WHERE batch.id = source.batch_id;

ALTER TABLE pool_batches
    ALTER COLUMN vta_image SET NOT NULL,
    ADD CONSTRAINT pool_batches_vta_image_check CHECK (vta_image <> '');
