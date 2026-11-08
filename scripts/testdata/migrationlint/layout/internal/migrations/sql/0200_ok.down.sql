DROP TRIGGER feature_flags_touch ON core.feature_flags;
DROP FUNCTION core.touch_flag();
ALTER TABLE sched.tasks DROP COLUMN flag_name;
DROP TABLE core.feature_flags;
