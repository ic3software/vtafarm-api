-- Email becomes the self-declared identifier for publicly signed-up accounts.
-- NULL for pre-existing and admin-invited accounts; unique when present so one
-- email can never map to more than one account.
ALTER TABLE users ADD COLUMN email VARCHAR(254);
CREATE UNIQUE INDEX idx_users_email ON users(email) WHERE email IS NOT NULL;

-- The admin-approval signup flow is gone: visitors now register directly from
-- the home page, so the request queue table has no readers or writers left.
DROP TABLE signup_requests;
