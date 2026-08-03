-- Adding co-admins to a stack's VTA after it is running. Design:
-- docs/platform-stack-admin-grant-design.md.
--
-- Until now the VTA's admin ACL was written exactly once, mid-pipeline, by
-- step_import_admin_did — and never again, so a second admin could only be added
-- by whoever already held the stack's credential running `pnm acl create` by
-- hand. This makes it self-service.
--
-- Adding only. Removal stays `pnm acl delete` against the live VTA: no downtime,
-- and nothing on this side has to work out which ACL entry belongs to whom.
--
-- What is recorded is only what was added from here. There is deliberately no
-- copy of the VTA's own admin list: reading it costs the same maintenance window
-- as writing it, any copy goes stale the moment a co-admin rotates their key,
-- and `pnm acl list` answers the live question against the running VTA for free.

-- ── The grants we made ──────────────────────────────────────────────────────
-- Keyed by session, not by "the platform stack", because the mechanism is
-- session-generic and only the routes are narrowed to the platform stack
-- (design §1). Generalising later is a route addition, not a migration.
--
-- Deliberately no role or contexts column. Every row is an unrestricted admin —
-- that IS the feature (design §2): the same `vta import-did --role admin` with
-- no --context that minted the stack's first admin. A column holding one value
-- forever is an invitation to put a second value in it without doing the
-- authorization work that would need.
CREATE TABLE vta_admin_grants (
    id           BIGSERIAL   PRIMARY KEY,
    -- CASCADE: these describe a store that is deleted with the session. There
    -- is nothing here that outlives it and nothing to orphan.
    session_id   BIGINT      NOT NULL REFERENCES setup_sessions(id) ON DELETE CASCADE,
    did          TEXT        NOT NULL,
    label        TEXT        NOT NULL DEFAULT '',
    -- pending is written BEFORE the Kubernetes work starts, so an HTTP client
    -- that times out mid-operation has not lost it: the row is the record, not
    -- the response body. It is also what a second API replica reads to refuse a
    -- concurrent grant, which the in-process lock alone cannot cover.
    --
    -- There is no 'revoked': removing an admin is `pnm acl delete` against the
    -- live VTA, not something this side does. See the design doc §1.
    status       TEXT        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'granted', 'failed')),
    error_msg    TEXT        NOT NULL DEFAULT '',
    -- Which admin asked. SET NULL rather than CASCADE: who was granted access
    -- is a fact about the VTA, and it must survive the departure of whoever
    -- requested it. This is the accountability half of accepting that a
    -- vtafarm admin cookie can now mint a VTA super admin (design §7.4).
    requested_by BIGINT      NULL REFERENCES admins(id) ON DELETE SET NULL,
    granted_at   TIMESTAMPTZ NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live grant per DID per session. Partial rather than total so a `failed`
-- attempt stays as history and the same DID can be tried again without
-- deleting the record of the first try.
CREATE UNIQUE INDEX vta_admin_grants_live_unique
    ON vta_admin_grants (session_id, did)
    WHERE status IN ('pending', 'granted');

CREATE INDEX vta_admin_grants_session_idx ON vta_admin_grants (session_id);
