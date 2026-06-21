-- Revert: Rename agent_session_id back to crush_session_id
ALTER TABLE tasks RENAME COLUMN agent_session_id TO crush_session_id;
