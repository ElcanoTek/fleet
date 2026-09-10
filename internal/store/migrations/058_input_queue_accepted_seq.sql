-- Acceptance sequence for the Stop scope=all sweep (#1477). created_at is
-- whole seconds, so a Stop could not tell a row accepted just BEFORE it from
-- one accepted just AFTER it in the same second: the sweep cancelled every
-- still-queued row, and the claim-limbo epoch gate had to be strict to let a
-- fresh post-Stop submission run. accepted_seq is a process-wide counter the
-- fleet process allocates at EnqueueInput (seeded from MAX(accepted_seq) at
-- boot, so it never repeats across restarts); a Stop records the counter's
-- value at the instant it begins, the sweep cancels exactly the rows
-- allocated at or below that value, and the launch gate refuses exactly the
-- same set. A counter rather than a wall-clock instant, because a clock
-- stepped backward after a Stop would otherwise make later, acknowledged
-- inputs look pre-Stop. Nullable: rows written by an older binary mid-deploy
-- have no value and read back as 0 — always inside any Stop's swept set,
-- which is the pre-#1477 behaviour for them. Existing rows are numbered in
-- acceptance order so their relative order survives.
ALTER TABLE chat_input_queue ADD COLUMN accepted_seq BIGINT;
UPDATE chat_input_queue q
   SET accepted_seq = numbered.seq
  FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, position, id) AS seq
          FROM chat_input_queue) AS numbered
 WHERE q.id = numbered.id AND q.accepted_seq IS NULL;
