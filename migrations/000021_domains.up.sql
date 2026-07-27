-- Domains a session can run under, beyond the generated names in our own zone.
--
-- Two kinds, and they are not variations of each other (design §3):
--   custom    the user owns the zone (aaa.com). We never touch their DNS —
--             they create the records, we verify them, and cert-manager issues
--             a certificate. Not reachable yet: the routes that create these
--             land in phase 3.
--   platform  our own zone, fixed labels (vta.firstperson.dev and friends).
--             No verification and no ACME — the *.firstperson.dev wildcard
--             already covers it. Created only by POST /admin/platform-stack.
--
-- Both share one property that shapes everything below: the four labels are
-- fixed, so a domain backs at most one session.

CREATE TABLE domains (
    id           BIGSERIAL    PRIMARY KEY,
    -- Owner. For platform rows this is the system account (design §3.3.6), not
    -- the admin who created it: admins live in a different table, and this id
    -- also derives the Kubernetes namespace.
    user_id      BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The registrable domain or any subdomain of it — 'aaa.com', not a host.
    domain       TEXT         NOT NULL,
    kind         VARCHAR(16)  NOT NULL,
    -- The expected TXT value. Minted per attach and checked live; custom only,
    -- so platform rows keep the default.
    verify_token TEXT         NOT NULL DEFAULT '',
    -- NULL until the check passes. Platform rows are verified on insert —
    -- we already control the zone.
    verified_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

ALTER TABLE domains ADD CONSTRAINT domains_kind_check
    CHECK (kind IN ('custom', 'platform'));

-- A domain exists once, globally: two accounts must never hold the same name.
-- Development and production never collide here because they are separate
-- databases (design §4.2).
CREATE UNIQUE INDEX domains_domain_unique ON domains (domain);

-- One custom domain per account. Partial so platform rows are exempt — the
-- system account holds exactly one of those and could otherwise never hold
-- anything else.
CREATE UNIQUE INDEX domains_one_custom_per_user
    ON domains (user_id) WHERE kind = 'custom';

-- RESTRICT rather than CASCADE: deleting a domain out from under a running
-- session would strand its DNS and its DIDs. The handler answers 409; this is
-- the backstop.
ALTER TABLE setup_sessions
    ADD COLUMN domain_id   BIGINT REFERENCES domains(id) ON DELETE RESTRICT,
    ADD COLUMN domain_type VARCHAR(16) NOT NULL DEFAULT 'managed';

ALTER TABLE setup_sessions ADD CONSTRAINT setup_sessions_domain_type_check
    CHECK (domain_type IN ('managed', 'custom', 'platform'));

-- domain_type is denormalised from domains.kind so mode dispatch stays a
-- single column read and the partial indexes below don't need a join.
-- domain_id IS NULL exactly when domain_type = 'managed'.
ALTER TABLE setup_sessions ADD CONSTRAINT setup_sessions_domain_link_check
    CHECK ((domain_type = 'managed') = (domain_id IS NULL));

-- A domain backs at most one session, because its four labels are fixed.
CREATE UNIQUE INDEX setup_sessions_domain_unique
    ON setup_sessions (domain_id) WHERE domain_id IS NOT NULL;

-- vta_name / vtc_name are hostnames only on the managed zone (design §4.3). On
-- fixed-label domains they appear in no hostname and survive only as a display
-- label in did:webvh paths, already made distinct by the hostname itself and
-- by their -vta / -mediator / -vtc suffixes — so duplicates across users are
-- fine there and these indexes must not see those rows.
--
-- The vtc_name predicate keeps 000020's shape: keyed on the value, not on a
-- mode string, because "a session that has a vtc_name owns it exclusively" is
-- the actual invariant and it survives the next mode rename.
--
-- Both names are load-bearing beyond the database — internal/handler/setup.go
-- matches on these exact index names to turn a constraint violation into a
-- 409 — so they are recreated under the same names.
DROP INDEX setup_sessions_vta_name_unique;
DROP INDEX setup_sessions_vtc_name_unique;

CREATE UNIQUE INDEX setup_sessions_vta_name_unique
    ON setup_sessions (vta_name) WHERE domain_type = 'managed';

CREATE UNIQUE INDEX setup_sessions_vtc_name_unique
    ON setup_sessions (vtc_name)
    WHERE domain_type = 'managed' AND vtc_name <> '';
