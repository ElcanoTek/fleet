-- Workspace file browser (#287): record the host filesystem path of the per-run
-- workspace directory a scheduled task's agent wrote into, so the file-browser
-- endpoints (GET /tasks/{id}/workspace, GET /tasks/{id}/workspace/{path}) can
-- list and stream the artifacts it produced. The runner sets this when the run
-- begins; NULL/empty = no workspace was recorded (legacy rows, or a run that
-- never reached execution).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS workspace_path TEXT;
