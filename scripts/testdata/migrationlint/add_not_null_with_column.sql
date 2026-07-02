-- Fixture (#593): the classic form the linter has always rejected — an
-- ADD COLUMN ... NOT NULL with no DEFAULT rewrites the table under an
-- ACCESS EXCLUSIVE lock.
ALTER TABLE tasks ADD COLUMN flag boolean NOT NULL;
