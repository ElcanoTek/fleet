# Chat escalation: offering the advanced model

**Status:** shipped 2026-10-15.

Chat runs on a fast everyday model (Claude Haiku 5.5 by default), which is the
right trade for most messages and the wrong one for some. This note covers how
fleet notices that a conversation needs the advanced model and offers it, at a
cost close to zero.

## What shipped

Four ways a chat on the everyday model reaches the advanced one. All of them
except **Retry with** use the same card (**Switch & retry**, **Just switch**,
**Dismiss**), the same gate, and the same cooldown.

| Source | What triggers it | Cost |
|---|---|---|
| The assistant | It calls `suggest_advanced_model` (unchanged; the tool description now also names repeated failures, and the generic chat prompt says when to hand up) | none |
| Struggle signals | End of turn: 3+ tool calls failed in a row, the loop guard blocked a repeated call, the context was compacted, or 25+ tool calls in the turn | none: read from the turn's own history and stream |
| Frustration check | A decision model (Jev by default) scores the user's latest message as pushback on a wrong or incomplete answer | ~$0.00002 per message (measured: ~460 input tokens on Jev 1.14) |
| **Retry with** | The user presses it under a reply (hidden in a lockdown chat whose allow-list does not name the advanced model) | none: it is a normal retry on the advanced model |

**The card gate** (`approvalStager.stageSuggestionGated`):

- no card on a conversation already on the advanced model;
- no card in a lockdown chat whose allow-list excludes the advanced model;
- no second card while one is pending;
- after a card is dismissed, or accepted and the user later moved off the
  advanced model, no new card until the user has sent 3 more messages since
  answering it (counted from the card's resolution, so time it sat pending
  does not count);
- at most one card per turn.

Accepting used to suppress a conversation for good, so a user who moved back to
the fast model was never offered the advanced one again.

**Host-staged cards** come after the reply is complete, on the same turn
stream, so they never interrupt the model. The check is the last post-turn
step, so the stream closes right after a card lands; a **Switch & retry**
pressed before it closes is held by the client and sent once it does
(`useQueuedSwitchRetry`), unless the user has moved to another conversation,
in which case it is dropped. The card's `args_json` records who
asked (`source`: `model`, `signal`, `classifier`), and the reason it shows
comes from the signal ("3 tool calls in a row failed on this request…").

### The frustration check

- **When it runs.** It starts with the turn and runs in parallel
  (`startStruggleCheck`), so it is normally finished before the reply is.
  Measured latency on Jev 1.14 was 0.1–0.6 s. A message the user steers in
  while the agent works is checked too, at the end of the turn, with the turn
  so far as context; the higher score counts.
- **It never holds the turn open.** After the reply, the stream waits at most
  1.5 s for the answers (the client blocks the next message while the stream
  is open). A later answer is recorded in the background for the per-user
  count, without a card, since the stream has closed
  (`fleet_escalation_classifier_checks_total{outcome="late"}`).
- **What it sends.** The last ~6,000 characters of user and assistant *text*:
  never tool output, attachments or the injected context. It goes through
  OpenRouter's Decisions API (`POST /api/alpha/decisions`), the same path Lens
  uses for custom categories.
- **What it asks.** One yes/no question with a probability: is the user
  pushing back on the assistant's own work? A user fixing their own request
  ("sorry, I meant Q2") is a no. The wording was tuned against live Jev:
  - clear pushback scored 0.95–0.99;
  - normal follow-ups, refinements and the user's own corrections scored
    0.02–0.11.
- **When it is skipped:**
  - the first message of a conversation;
  - turns fleet started itself (a resume after an approval);
  - lockdown conversations;
  - deployments with no OpenRouter key.
- **On the advanced model** it still runs, so the per-user count is complete,
  but never shows a card.
- **Modes** (admin setting):
  - `suggest`, the default: score, count, and offer the card.
  - `observe`: score and count, no card.
  - `off`: nothing is sent. The check sends recent chat text to a second model
    vendor (the decision model's provider, through OpenRouter); a deployment
    that must not do that sets `off` (`FLEET_ESCALATION_CLASSIFIER_MODE=off`).
- **Model.** Any OpenRouter decision model. The default, `~typesafe/jev-latest`,
  is a moving alias, so a new Jev release needs no change. The admin panel lists
  every paid decision model OpenRouter offers, live and cached for an hour
  (`GET /admin/decision-models`), so a newly released one is selectable without
  a fleet update. Any slug can also be typed.
- **Failures** (no key, a non-200, a malformed answer) mean no card and no
  record, plus a `fleet_escalation_classifier_checks_total{outcome="error"}`
  increment. A turn is never blocked or delayed by it.

### Records and metrics

- **`escalation_events`** (migration 078) gets one row per struggle:
  - every signal that fired, and every classifier "yes";
  - with the user, the conversation, the signal, the probability and whether a
    card was shown;
  - recorded whether or not a card was shown, so the count is how often users
    struggled, not how often fleet said so;
  - no message text is stored.
- **The admin Users page** (Settings → Admin → Users) shows per user:
  - **Frustrated**: classifier yes, counted only while the check is on;
  - **Struggles**: signal rows;
  - **Switched up**: accepted suggestions.

  Counts are all-time and follow conversation deletion (`ON DELETE CASCADE`,
  like `turn_metrics`).
- **Metrics:**
  - `fleet_model_escalations_total{source, outcome}`, where outcome is
    `staged`, `suppressed`, `error`, `dismissed`, or `accepted_switch_and_retry`
    / `accepted_switch_only`;
  - `fleet_escalation_classifier_checks_total{mode, outcome}`, where outcome is
    `pushback`, `ok`, `error` or `late`.
- **The approvals table** now records which accept action was taken, in the
  result text.

## Fixed along the way

- **Switch & retry retried on the old model.**
  - *Cause:* the card pinned the advanced model, then called the transcript's
    retry from the previous render, whose `selectedModel` was still the old
    model.
  - *Effect:* the retry's turn request carried that model, and the server
    re-pinned it.
  - *Fix:* the card now passes the pinned model to the retry
    (`retryLastUserMessage({ model })`), and **Retry with** uses the same path.
- **The admin copy for the Advanced model** said a change applies "to
  suggestion cards already on screen". It does not: a card freezes its target
  when staged (`TestSuggestionFreezesConfiguredTargetAtStaging`). The copy now
  says so.

## Not done

- **Retry with** and the spreadsheet banner are client-side and are not
  counted server-side.
- The signal thresholds (3 failures, 25 tool calls) are constants, not admin
  settings. They are deliberately conservative. Make them settings if the
  Users page shows they fire too often or too rarely.
- No direct-to-TypeSafe fallback when OpenRouter's Decisions API fails, unlike
  Lens. A failed check only means no card, so the extra key was not worth it.
- No automatic escalation of scheduled tasks; those default to the task tier
  ([TASK-MODEL-TIER.md](TASK-MODEL-TIER.md)).
- The Decisions API is alpha on OpenRouter. If its shape changes, the check
  fails closed (no card) and the error counter shows it.
