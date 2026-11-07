-- 075_add_dataset_creator.down.sql — drop the dataset creator columns.
ALTER TABLE datasets DROP COLUMN IF EXISTS created_by_key_id;
ALTER TABLE datasets DROP COLUMN IF EXISTS created_by_user_id;
