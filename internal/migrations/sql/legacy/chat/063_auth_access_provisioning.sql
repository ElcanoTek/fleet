-- Central Auth controls whether an identity may enter Fleet. Disabling is
-- intentionally non-destructive: roles, teams, conversations, and account
-- data remain for a later re-grant.
ALTER TABLE users ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;

CREATE TABLE external_access_state (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    version BIGINT NOT NULL,
    allowed BOOLEAN NOT NULL,
    event_id TEXT NOT NULL,
    chat_role TEXT NOT NULL CHECK (chat_role IN ('member', 'viewer', 'admin')),
    ops_role TEXT NOT NULL CHECK (ops_role IN ('none', 'readonly', 'client', 'admin')),
    issued_at BIGINT NOT NULL,
    received_at BIGINT NOT NULL,
    PRIMARY KEY (issuer, subject)
);

CREATE INDEX external_access_state_email_idx ON external_access_state (email);
