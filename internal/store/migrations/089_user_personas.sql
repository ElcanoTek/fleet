-- 089_user_personas.sql — personas a user writes for themselves
-- (docs/USER-PERSONAS.md).
--
-- A row is one persona its owner wrote in Settings → Personas, or one the
-- persona_studio tool drafted for them (status 'proposed', inert until the
-- owner approves it). Unlike bundle personas — operator content read from
-- the client-config bundle's personas/ directory — a user persona is
-- DB-owned and only ever runs in its owner's conversations, which reference
-- it as "user:<name>".
--
-- base_persona names the bundle persona the user persona builds on: its YAML
-- is still composed into the prompt and its manifest tool policy still
-- applies, so a user persona can add instructions but never widens what the
-- operator granted. '' = the deployment default persona.
--
-- patrols is a JSON array of suggested in-chat patrols ({title, cron,
-- timezone, prompt}) the owner can start from the persona page in one click;
-- they are suggestions, not schedules — nothing fires until started.
--
-- replaces_name is set on a proposal that edits an existing persona: approving
-- it writes the draft onto that persona and drops the draft row.
CREATE TABLE IF NOT EXISTS user_personas (
    id            TEXT PRIMARY KEY,
    user_email    TEXT NOT NULL,
    name          TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    instructions  TEXT NOT NULL,
    base_persona  TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'assistant',
    autonomy      TEXT NOT NULL DEFAULT 'advise',
    patrols       JSONB NOT NULL DEFAULT '[]'::jsonb,
    status        TEXT NOT NULL DEFAULT 'active',
    replaces_name TEXT NOT NULL DEFAULT '',
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_personas_user_name
    ON user_personas (user_email, name);
