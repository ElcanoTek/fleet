-- Drop the per-conversation runtime-flavor column. The runtime-"flavor" system
-- (and the ACP runtimes it selected) has been removed: fleet now runs exactly
-- one native in-process agent loop, so there is nothing to select. The historical
-- 009_conversation_runtime migration is left intact (the ledger is append-only);
-- this forward migration removes the now-unused column.

ALTER TABLE conversations
    DROP COLUMN IF EXISTS runtime;
