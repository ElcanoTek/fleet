-- Fixture (#593): Postgres makes the COLUMN keyword optional, so this is the
-- SAME dangerous DDL as the ADD COLUMN form — it must not slip past the gate.
ALTER TABLE tasks ADD flag boolean NOT NULL;
