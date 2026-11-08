-- Team renames central Auth sends (docs/CENTRAL-AUTH-PROVISIONING.md, "Team
-- renames"): the last version applied per issuer, so a redelivered or older
-- rename is a no-op. Auth sends its renames in order, one at a time, so the
-- next version is never applied before this one.
CREATE TABLE external_team_renames (
    issuer TEXT PRIMARY KEY,
    version BIGINT NOT NULL,
    event_id TEXT NOT NULL,
    from_team TEXT NOT NULL,
    to_team TEXT NOT NULL,
    applied_at BIGINT NOT NULL
);
