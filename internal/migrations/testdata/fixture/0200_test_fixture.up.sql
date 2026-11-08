-- A test-only migration in the new layout (internal/migrations tests): it
-- names every object with its schema, as new migrations must.
CREATE TABLE core.m6_fixture (
    id   integer PRIMARY KEY,
    note text NOT NULL DEFAULT ''
);
INSERT INTO core.m6_fixture (id, note) VALUES (1, 'seeded by 0200');
ALTER TABLE sched.tasks ADD COLUMN m6_fixture_note text;
