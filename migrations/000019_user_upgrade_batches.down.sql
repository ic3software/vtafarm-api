ALTER TABLE upgrade_batches DROP CONSTRAINT upgrade_batches_one_initiator;
-- admin_id becomes NOT NULL again — user-initiated batches can't survive.
DELETE FROM upgrade_batches WHERE admin_id IS NULL;
ALTER TABLE upgrade_batches DROP COLUMN user_id;
ALTER TABLE upgrade_batches ALTER COLUMN admin_id SET NOT NULL;
