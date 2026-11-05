ALTER TABLE upgrade_batches
    DROP CONSTRAINT upgrade_batches_user_id_fkey,
    ADD CONSTRAINT upgrade_batches_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
