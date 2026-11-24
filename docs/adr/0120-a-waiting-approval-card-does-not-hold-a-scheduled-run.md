# ADR-0120: A waiting approval card does not hold a scheduled patrol run

- **Status:** Accepted
- **Date:** 2026-11-24
- **Deciders:** fleet maintainers
- **Reverses:** the delivery rule in [`docs/CHAT-PATROLS.md`](../CHAT-PATROLS.md) since in-chat patrols shipped on 2026-10-28 (a scheduled fire is skipped while "a pending non-preview approval card" waits; restated on 2026-11-18 as "a patrol never stacks turns behind an unanswered card"). No earlier ADR recorded it.

## Context

A chat can run a recurring patrol or a one-shot timer: a scheduled prompt
that runs as a turn in that chat
([ADR-0104](0104-one-scheduler.md), [`docs/CHAT-PATROLS.md`](../CHAT-PATROLS.md)).
The chat side of a fire, `store.DeliverChatScheduleFire`, turned a **scheduled**
fire away (`busy`, reason `approval_pending`) whenever the schedule's owner had
a pending approval card in that chat, other than an email preview or a model
suggestion. A recurring patrol's slot was then skipped and not run later; a
timer retried every 30 seconds until the card was settled. **Run now** was
never held this way.

The rule was deliberate. A card is a question a person has not answered, and
the reasoning was that an unattended run should not stack more work, and
possibly more cards, behind it.

In use it did more harm than the stacking it prevented:

- **A once-daily 6:30 patrol** (2026-11-24). A trader asked for a change in
  the same chat in the afternoon and left its card unanswered. A card waits
  24 hours by default, so the whole of the next morning's run would have been
  skipped to protect one card that had nothing to do with the patrol.
- **A daily 8:00 report** (2026-11-24) was skipped because a card its own
  previous run had staged was still unanswered. That card timed out nine
  minutes after the slot. There was no report that day.
- **A two-hourly patrol** (2026-11-18) skipped its 12:00, 14:00 and 16:00
  slots behind one card staged at 10:00. That case led to the "Patrol on
  hold" note on the Patrol bar, which told the owner the runs were being
  skipped but did not run them.

Three facts made the hold unnecessary as well as costly:

- **An approval card already outlives turns.** It resolves outside any turn
  ([`docs/RESUME-AFTER-APPROVAL.md`](../RESUME-AFTER-APPROVAL.md)): a person
  can type a new message while a card waits, and that turn does not cancel
  it. A scheduled turn is no different.
- **The resume rules already cover a card settled while another turn is in
  flight.** The claim of settled cards and the insert of the one `resume`
  queue row are one transaction ([ADR-0083](0083-a-settled-approval-card-may-start-a-turn.md)),
  and the row waits in the queue behind whatever is running.
- **A patrol whose card is never answered does not chain.** A card that
  times out starts no resume turn in a chat that has a schedule; the next run
  picks the work up.

## Decision

**A pending approval card never turns a scheduled fire away.**

1. `store.DeliverChatScheduleFire` does not look for a pending card. A scheduled
   fire is `busy` only when a patrol turn of the owner's is already queued or
   running in the chat (`patrol_in_flight`) or the chat's input queue is full
   (`queue_full`). This holds for recurring patrols and one-shot timers, in
   every approval mode (the delivery reads no mode).
2. **The fire does not touch the card.** It stays pending, keeps its
   `expires_at`, its resume arming and its id, is not re-staged, and is
   approved, declined or timed out exactly as before. (A model-suggestion
   card is still superseded when a patrol turn is queued, as it was.)
3. **The run is told what is waiting.** A patrol turn's injected context
   lists the pending cards of the person the turn runs as
   (`appendWaitingApprovalsBlock`): tool, a one-line summary, approval id,
   when it was staged and when it expires. It tells the run that none of them
   has run, not to stage or run the same action again, to report them as
   waiting rather than done, and that a new card for the same tool replaces
   the waiting one: the run is to do that only to bring an earlier scheduled
   run's own card up to date, never to a card a person asked for, and to say
   which card it replaced. The list is bounded (12 cards, the rest counted by tool)
   and its text is bounded and made safe to quote. A run is not started
   without it: if the cards cannot be read, the queued turn goes back to the
   queue.
4. **How cards are superseded, expire and resume does not change.** In
   particular the stager's same-tool rule stands: a card a patrol turn stages
   for a tool replaces a pending card for that tool that the same person has
   from an earlier turn. That is right for a patrol's own cards (today's
   report card replaces yesterday's instead of one stale card per run). For
   a card a person asked for it is what any later turn of theirs would do,
   but a scheduled run does it with nobody there, so the run is told, before
   it starts, which cards a new call would replace and not to replace a
   person's; the tool result says how many were replaced.
5. **The "Patrol on hold" reporting is removed**, not left unreachable: the
   Patrol bar note, the Patrols page chip, the `skipped` field on the
   schedules and patrols responses, and the store reads behind them. The
   `approval_pending` reason stays as a constant and in the run-history CHECK
   (migration 0203) because rows recorded before this decision carry it, and
   a patrol's **Recent runs** still renders them.

## Enforcement

- `internal/store/chat_schedules_approvals_test.go` pins the delivery against
  the real database and scheduler claim: a scheduled fire with a card waiting
  queues its turn and the card is as it was staged (status, deadline,
  staging time, resume arming)
  (`TestScheduledFireRunsWithAnApprovalCardWaiting`, which also pins that
  `patrol_in_flight` and `queue_full` still turn a fire away and that no
  `approval_pending` row is written); the same for a timer, for every
  approval mode and for a multiplayer chat; a card approved while the patrol
  turn is queued or running yields one resume row behind it
  (`TestApprovalSettledWhileThePatrolTurnIsInFlight`); a card that times out
  mid-run starts no turn and leaves the run alone
  (`TestApprovalTimesOutWhileThePatrolTurnRuns`).
- `internal/httpapi/chat_approvals_context_test.go` pins what the run is told
  (`TestWaitingApprovalsBlock…`), that it is told only its owner's cards and
  is not started without them (`TestPatrolTurnIsToldTheCardsWaitingOnItsOwner`),
  the live case end to end through the real agent loop
  (`TestPatrolRunsWhileACardWaits_ThenTheResumeFollowsIt`), and what a patrol
  turn's own calls do to waiting cards in Manual, Auto-approve and Autonomous
  (`TestPatrolTurnStagingBesideWaitingCards`).
- `TestGetUserPatrolAndRuns` (store) and the mocked Playwright sidebar spec
  keep an old `approval_pending` history row readable.

Nothing mechanical stops a future change from adding the gate back; this
record and the tests above are what say it was removed on purpose.

## Consequences

- A patrol runs on its schedule whether or not anyone has answered a card.
  The three cases above run.
- **Unattended spend is no longer paused by an unanswered card.** A patrol in
  a chat nobody is reading keeps running each slot, as a patrol that stages
  no card always did. The existing bounds are unchanged: one patrol turn of
  an owner's at a time per chat, the queue's depth cap, each turn's cost and
  token ceilings, and the pause on archive.
- **Cards and runs now overlap, so the model has to be told.** Step 3 is an
  instruction to a model, not a mechanism: a run can still ignore it. The
  mechanisms that remain are the ones that were already there: a waiting
  card is replaced only by a new card for the same tool from the same person
  (the replaced card and the tool result both say so), and nothing a patrol
  turn does approves a card. A run that ignores the note can therefore
  replace a card its owner asked for with the same tool; the owner finds it
  marked superseded and has to ask again.
- **In Auto-approve or Autonomous, a call the mode covers runs without a
  card**, so it supersedes nothing: a card waiting for the same tool stays
  pending, and a run that repeated its action would do it once now and once
  more if the card is later approved. The same was already true of a turn a
  person typed in such a chat; the note tells the run not to.
- A resume released by approving a card waits, as before, until no other
  card of that person's is pending in the chat. With patrols no longer held,
  a chat can hold a card a person asked for and a card a run staged at the
  same time, so approving one may not continue until the other is settled.
- The Patrol bar and the Patrols page lose the "on hold" note and chip. An
  older web bundle that still reads `skipped` finds it absent and shows
  nothing.

## Alternatives considered

- **Run the skipped slot later, once the card is settled.** Keeps the hold
  and adds a catch-up. A 6:30 report that runs at 15:00 is not the report
  that was asked for, the catch-up needs its own rules (how late, how many),
  and the card is still unrelated to the run in the first case above.
- **Hold only for cards the patrol's own runs staged.** It would have fixed
  the first case and not the second or third, and needs the staging turn
  recorded on every card.
- **Let a patrol turn's cards never supersede an earlier card.** It would
  protect a card a person asked for from a run that uses the same tool, at
  the cost of a recurring patrol stacking one card per run for the same
  action. Protecting only a person's cards needs the staging turn's kind on
  every card, which fleet does not record. Superseding is a rule of the
  approval stager, shared by every kind of turn; changing it for one kind is
  a separate decision, and the place to make the note's rule a mechanism if
  runs prove not to follow it.
- **Keep the "Patrol on hold" note for boxes with old rows.** The note
  described a hold that no longer exists; shown after an upgrade it would
  tell the owner to answer a card to resume a patrol that is already running.
