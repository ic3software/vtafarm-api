-- Dropping this loses only throttle bookkeeping: verified_at is what decides
-- whether a domain may back a session, and it is untouched.
ALTER TABLE domains DROP COLUMN last_checked_at;
