-- Reverse 042_add_task_output_schema.
ALTER TABLE tasks DROP COLUMN IF EXISTS output_json;
ALTER TABLE tasks DROP COLUMN IF EXISTS output_schema;
