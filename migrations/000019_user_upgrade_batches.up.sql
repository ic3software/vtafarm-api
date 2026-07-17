-- User-initiated upgrades: a user can change the images of their own
-- sessions, so upgrade_batches gains a user_id initiator. Exactly one of
-- admin_id / user_id is set — a batch is either an admin rollout or one
-- user's self-service upgrade of a single session they own.

ALTER TABLE upgrade_batches ALTER COLUMN admin_id DROP NOT NULL;
ALTER TABLE upgrade_batches ADD COLUMN user_id BIGINT REFERENCES users(id);
ALTER TABLE upgrade_batches
    ADD CONSTRAINT upgrade_batches_one_initiator
    CHECK ((admin_id IS NULL) <> (user_id IS NULL));
