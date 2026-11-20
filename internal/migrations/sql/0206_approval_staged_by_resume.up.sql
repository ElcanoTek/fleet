-- 0206_approval_staged_by_resume: remember, on the card itself, that a resume
-- turn staged it.
--
-- A card that times out starts one unattended resume turn. When that turn
-- stages a card of its own and nobody answers it either, its timeout must not
-- start another turn, or an abandoned chat pays for one turn per expiry. The
-- claim needs to know which turn staged the card at the moment the card
-- expires, which can be days later: the turn and input-queue rows may be gone
-- by then (their retention sweeps are shorter than the longest timeout), so
-- the fact is recorded here when the card is armed.
ALTER TABLE chat.approvals
    ADD COLUMN IF NOT EXISTS staged_by_resume BOOLEAN NOT NULL DEFAULT FALSE;
