-- Rename crush_session_id to agent_session_id
ALTER TABLE tasks RENAME COLUMN crush_session_id TO agent_session_id;
