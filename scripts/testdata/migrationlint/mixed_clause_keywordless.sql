-- Fixture (#593): a DEFAULT on one column clause must not mask a keyword-less
-- NOT-NULL-without-default sibling clause.
ALTER TABLE tasks
    ADD a boolean NOT NULL DEFAULT false,
    ADD b boolean NOT NULL;
