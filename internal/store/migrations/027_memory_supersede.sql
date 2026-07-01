-- 027_memory_supersede.sql — contradiction candidates (#515 stage 2).
--
-- A memory PROPOSAL may claim it supersedes an existing saved memory (the
-- post-turn extractor flags "this new fact contradicts/outdates that one").
-- The claim is only ever acted on by a HUMAN Accept: the superseded memory is
-- then soft-retired with retired_by linking its replacement (026's columns).
-- supersedes_hash snapshots the target's content at claim time, so a target
-- edited between proposal and accept is NOT retired on a stale justification
-- (the accept surfaces "target changed" instead). Nothing retires silently.
ALTER TABLE memories ADD COLUMN IF NOT EXISTS supersedes TEXT;
ALTER TABLE memories ADD COLUMN IF NOT EXISTS supersedes_hash TEXT;
