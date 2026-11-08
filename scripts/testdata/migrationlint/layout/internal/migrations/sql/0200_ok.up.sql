-- A well-formed new migration: every name carries its schema.
CREATE TABLE core.feature_flags (
    name       text PRIMARY KEY,
    enabled    boolean NOT NULL DEFAULT false,
    owner_id   uuid REFERENCES core.users (id)
);
CREATE INDEX feature_flags_enabled ON core.feature_flags (enabled);
ALTER TABLE sched.tasks ADD COLUMN flag_name text;
INSERT INTO core.feature_flags (name) VALUES ('m6') ON CONFLICT DO NOTHING;
UPDATE sched.tasks SET flag_name = 'm6' WHERE flag_name IS NULL;
CREATE FUNCTION core.touch_flag() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER feature_flags_touch BEFORE UPDATE ON core.feature_flags
    FOR EACH ROW EXECUTE FUNCTION core.touch_flag();
-- prose that names a bare table is a comment: CREATE TABLE ignored (x);
