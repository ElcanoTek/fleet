-- Fixture (#593): safe DDL the linter must NOT flag — every ADD form that is a
-- table constraint (not a column add), NOT NULL adds with a DEFAULT (both with
-- and without the COLUMN keyword), and the non-destructive DROP actions inside
-- ALTER TABLE.
ALTER TABLE tasks ADD COLUMN flag boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD flag2 boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN note text;
ALTER TABLE tasks ADD note2 text;
ALTER TABLE tasks ADD CONSTRAINT tasks_flag_chk CHECK (flag IS NOT NULL);
ALTER TABLE tasks ADD PRIMARY KEY (id);
ALTER TABLE tasks ADD UNIQUE (name);
ALTER TABLE tasks ADD FOREIGN KEY (owner_id) REFERENCES users(id);
ALTER TABLE tasks ADD CHECK (retries >= 0);
ALTER TABLE tasks ALTER COLUMN flag SET NOT NULL;
ALTER TABLE tasks ALTER COLUMN flag DROP DEFAULT;
ALTER TABLE tasks ALTER COLUMN flag DROP NOT NULL;
ALTER TABLE tasks DROP CONSTRAINT tasks_flag_chk;
CREATE TABLE widgets (
    id bigint PRIMARY KEY,
    name text NOT NULL
);
