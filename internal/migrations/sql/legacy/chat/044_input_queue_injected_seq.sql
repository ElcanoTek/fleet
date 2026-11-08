-- Injection watermark for at-most-once steer settlement (#823). injected_seq
-- records the turn journal's max seq at the moment MarkInputInjected flipped
-- the row queued -> injected. Settlement and boot recovery requeue an injected
-- row whose turn never committed history ONLY when no tool intent was
-- journaled after this watermark — i.e. the model provably never dispatched a
-- tool after seeing the steer, so re-running it cannot duplicate a side
-- effect. NULL (rows injected before this migration) degrades to the coarse
-- gate: any tool intent for the turn blocks the requeue.
ALTER TABLE chat_input_queue ADD COLUMN injected_seq BIGINT;
