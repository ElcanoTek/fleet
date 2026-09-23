-- Central Auth revocation must preserve the Operations Center identity UUID,
-- role, and task ownership for a later re-grant.
ALTER TABLE users ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;
