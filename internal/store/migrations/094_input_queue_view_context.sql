-- The presentation context a queued chat submission was sent from
-- (docs/DASHBOARD-VIEW.md): JSON {"view": "dashboard", "board": [...]} when
-- the sender had the chat open in dashboard view, so the turn that drains the
-- row tells the agent about the board exactly as a direct send does. NULL for
-- every other row (and for rows queued before this migration), which drains
-- with no dashboard note, as before. Opaque to the store.
ALTER TABLE chat_input_queue ADD COLUMN IF NOT EXISTS view_context TEXT;
