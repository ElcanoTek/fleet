-- Creates a table, then fails: nothing of it may survive, and no ledger row.
CREATE TABLE core.m6_half_done (id integer);
SELECT no_such_column FROM core.m6_half_done;
