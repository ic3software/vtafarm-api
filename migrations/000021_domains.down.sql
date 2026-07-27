-- Restore the pre-domains indexes first: they are unconditional, so any
-- fixed-label session still holding a duplicate vta_name/vtc_name would make
-- them fail to build. Dropping the table below removes those sessions' domain
-- link but not the sessions, so de-duplicate by hand if this errors.
DROP INDEX setup_sessions_vta_name_unique;
DROP INDEX setup_sessions_vtc_name_unique;

CREATE UNIQUE INDEX setup_sessions_vta_name_unique ON setup_sessions (vta_name);

CREATE UNIQUE INDEX setup_sessions_vtc_name_unique ON setup_sessions (vtc_name)
    WHERE vtc_name <> '';

DROP INDEX setup_sessions_domain_unique;

ALTER TABLE setup_sessions DROP CONSTRAINT setup_sessions_domain_link_check;
ALTER TABLE setup_sessions DROP CONSTRAINT setup_sessions_domain_type_check;
ALTER TABLE setup_sessions DROP COLUMN domain_type;
ALTER TABLE setup_sessions DROP COLUMN domain_id;

DROP TABLE domains;
