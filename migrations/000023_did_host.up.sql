-- did_host records which DID-hosting daemon a session's did:webvh identifiers
-- are served by. It exists to scope one uniqueness rule correctly, and that
-- rule is the whole reason the column is here.
--
-- A did:webvh path only has to be distinct among the DIDs sharing a daemon.
-- Which daemon that is does NOT follow from mode or from domain_type:
--
--   vta_only       has no daemon of its own — its DID is uploaded to the
--                  shared one (DID_HOSTING_SERVER_URL, which is the platform
--                  stack's). True whether its hostnames are ours or, once
--                  custom + vta_only is allowed, the user's own: a custom
--                  domain moves the hostnames, not the DID host.
--   full_stack     runs its own daemon at dids[-<name>].<zone>, so its three
--                  paths share a namespace with nothing else.
--   platform       is a full_stack whose own daemon IS the shared one, so its
--                  paths sit alongside every vta_only session's.
--
-- Storing the host rather than deriving it also keeps the record honest when
-- DID_HOSTING_SERVER_URL is repointed: the DIDs those rows minted really are
-- still served by the old daemon.

ALTER TABLE setup_sessions ADD COLUMN did_host TEXT NOT NULL DEFAULT '';

-- Backfill. Both branches recover the host exactly rather than guessing:
-- vta_only rows carry it inside the vta_did_url they were built with, and a
-- full_stack row's daemon is the dids hostname it already stores.
UPDATE setup_sessions
   SET did_host = split_part(split_part(vta_did_url, '://', 2), '/', 1)
 WHERE mode = 'vta_only' AND vta_did_url <> '';

UPDATE setup_sessions
   SET did_host = dids_subdomain || '.' || domain
 WHERE mode = 'full_stack' AND dids_subdomain <> '';

-- One index for one namespace: paths on the same daemon must not collide.
--
-- It compares vta_name rather than the rendered path because every indexed
-- path is <vta_name>-vta — vta_only included, which is the point of giving it
-- the same suffix full_stack uses. That makes the comparison exact: a row's
-- other two paths (<vta_name>-mediator / -vtc) are unindexed, but they end in
-- suffixes no -vta path can equal, so nothing can slip past by being compared
-- under the wrong name.
--
-- Partial on did_host <> '' so a row whose daemon is unknown (nothing creates
-- one, but the backfill above can leave one behind for a session torn down
-- mid-flight) is left out rather than colliding with every other such row.
CREATE UNIQUE INDEX setup_sessions_did_path_unique
    ON setup_sessions (did_host, vta_name) WHERE did_host <> '';
