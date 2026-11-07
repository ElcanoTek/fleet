-- 075_add_dataset_creator.up.sql — record who created each dataset.
--
-- Datasets now follow the same own-rows model as tasks (#1082): a principal
-- reads and changes the datasets it created, and the fleet-wide grant
-- (admin, view_all_logs) covers every dataset. created_by_user_id is the
-- creating user (tasks.created_by), created_by_key_id the creating API key
-- (tasks.created_by_key_id). Both are NULL for every dataset created before
-- this column existed; such a dataset is reachable only through the
-- fleet-wide grant, the same as an ownerless task.
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS created_by_user_id UUID;
ALTER TABLE datasets ADD COLUMN IF NOT EXISTS created_by_key_id TEXT;
