-- Turn-scoped input-queue lookups (#785). SettleTurnInputs, BindInputTurn,
-- CompleteInjectedInputs, and boot recovery all filter chat_input_queue by
-- turn_id on every turn completion; 042 only indexed the idempotency key and
-- the pending drain order, so those ran as sequential scans on a table that
-- grows with every busy-submission.
CREATE INDEX chat_input_queue_turn ON chat_input_queue (turn_id) WHERE turn_id IS NOT NULL;
