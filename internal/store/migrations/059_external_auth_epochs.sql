-- Independent revocation generations for centrally authenticated Fleet
-- sessions. Fleet password sessions continue to use the users.password_hash
-- epoch; a central logout rotates only this table.
CREATE TABLE external_auth_epochs (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    epoch TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (issuer, subject)
);

CREATE INDEX external_auth_epochs_email_idx ON external_auth_epochs (email);

CREATE TABLE external_logout_events (
    event_id TEXT PRIMARY KEY,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    received_at BIGINT NOT NULL
);
