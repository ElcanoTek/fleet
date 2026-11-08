CREATE TABLE feature_flags (name text PRIMARY KEY);
CREATE INDEX feature_flags_name ON feature_flags (name);
INSERT INTO feature_flags (name) VALUES ('x');
