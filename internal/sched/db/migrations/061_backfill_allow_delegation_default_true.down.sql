-- 061 down — restore the pre-#1043 column default. The backfill itself is not
-- reversible (pre-migration explicit-true rows are indistinguishable from
-- backfilled ones), so rows keep allow_delegation=true; only the default for
-- future inserts reverts.
ALTER TABLE tasks ALTER COLUMN allow_delegation SET DEFAULT FALSE;
