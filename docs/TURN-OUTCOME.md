# Turn outcome — a server-side answer the client reads instead of inferring

`GET /conversations/{id}/turns/{turn_id}` reports what became of one turn.
It is a **read over state the server already had**: no new table, no new
column, no migration, and nothing written on the turn path. Issue #1593.

Companion to [`CHAT-STREAM-RECOVERY.md`](CHAT-STREAM-RECOVERY.md), which
covers the client machinery this feeds.

## The question the client could not ask

Chat recovery decided what happened to a turn by combining two probes that
answer *different* questions:

- `GET /conversations/{id}/inflight` — "is anything running on this
  conversation?"
- the conversation history — "is there an answer?"

Neither is about **a particular turn**, and one shape is genuinely undecidable
from their combination:

> Nothing is live, nothing is retained, and the transcript ends at the user's
> prompt.

That is what a turn which failed **before producing a reply** leaves behind — a
`turn.model_required`, a pre-answer provider error — and it is byte-for-byte
what a turn that simply produced nothing leaves behind. The client had to
choose one reading for both. On a mid-flight slot it chose "the connection
dropped before the response finished", which offers Retry but blames the wrong
thing; on a slot already reading `done` after a replay gap it chose nothing at
all, leaving a blank reply with **no Retry** — the wrong affordance for a
failure. (Found by the Codex review on #1584, round 14.)

## What the server already knew

Every fact the endpoint reports predates it:

| Fact | Where it has always lived |
| --- | --- |
| terminal outcome | `turns.status` — `running` / `completed` / `cancelled` / `error`, since migration 002 |
| why it ended | the terminal frame in `turn_events` (`turn.completed` / `turn.cancelled` / `turn.error` / `turn.model_required`) |
| is the prompt on record | `messages (turn_id, turn_seq = 1)`, written by `CommitUserMessage` before the first provider call |

`inferTerminalStatus` already maps `turn.model_required` to
`TurnStatusError`, so a pre-answer model failure has been a recorded failure
all along — the client just had no way to read it.

## The endpoint

```
GET /conversations/{id}/turns/{turn_id}

{ "state": "running", "user_committed": false }

{ "state": "failed",
  "reason": "model_required",
  "detail": { "reason": "retry_exhausted", "failed_model": "…", "message": "…" },
  "user_committed": true,
  "finished_at": 1758531234 }
```

- **`state`** is the client's vocabulary, not the row's: `error` is a status,
  `failed` is what the UI decides about, and a turn still generating has no
  terminal row at all. An unrecognised status reports `running` — a client that
  waits and re-asks is recoverable, one that stamps a wrong verdict is not.
- **`reason`** names the frame that sealed the turn. `model_required` is broken
  out from `error` because the engine emits `turn.model_required` *instead of*
  `turn.error` for a failure the user can fix by picking another model.
- **`detail`** is that frame's payload **verbatim** — the same JSON the SSE
  stream carried — so the client needs no second vocabulary for it, and
  `applyTurnOutcome` can hand a `model_required` outcome straight to the
  existing `applyModelRequired`.
- **`user_committed`** closes the registered-before-committed window: a turn is
  registered and exposed *before* its user message commits, so `false` on a
  running turn means the transcript legitimately does not hold this turn's
  prompt yet.
- **404** means the server has no such turn for this caller's conversation.
  That is "no answer available", not silence — see the reachability rule below.

Ownership is the conversation's, resolved exactly as every other conversation
sub-route resolves it, and then folded into the turn lookup's own `WHERE`
clause, so a caller cannot read another conversation's turn even if the
handler's check were later dropped (#1112).

### The one place memory beats the row

The in-memory registry can override a terminal row with `running`, never the
reverse. The two disagree for exactly one reason: `turnBuffer.Finish` seals the
buffer first and issues `FinishTurn` last, so in between a turn is over in
memory while the row still says running. Reporting `running` there is the safe
read. The reverse — calling a registered, running turn finished because a row
had not caught up — is the verdict-from-an-absence this whole area exists to
prevent.

## What the client does with it

`settleStreamedSlot` is the one finalizer for a slot a dead socket left open.
It still asks Postgres first; only when Postgres has **no answer** does it now
ask what became of the turn:

- **`failed`** → stamp the failure with the server's own cause, and offer
  Retry. A `model_required` outcome rebuilds the model-picker banner the live
  stream would have shown.
- **`cancelled`** → cancelled, not failed.
- **`running`** → not a verdict. Leave the slot mid-flight and re-arm the
  recovery chain.
- **`completed`**, or nothing to ask about → the pre-#1593 behavior, unchanged.

Three reachability answers, and the third is not a failure of the second:
`answer`, `unreachable` (thrown fetch, 5xx, an expired session — re-ask later)
and `unknown` (no turn id, or a 404 — re-asking would never resolve it, so fall
back rather than wait forever).

A slot holding a **pending approval or memory proposal** is decided *before*
the outcome is consulted. Such a turn really is still running server-side, so
asking would only re-arm the chain for as long as the card sits unanswered —
and a "Turn failed / Retry" banner over a live action card tells the reader to
throw away the very decision being asked for.

## Deliberately not shipped

The issue named three client uses. One shipped; the other two are entangled
with its siblings and are **not** in this change:

- **Dropping the prompt-text matching in the deferred hand-off.**
  `performDirectHandoff` compares its submitted prompt against the newest user
  row because it has no turn id to ask about — the direct response's body is
  cancelled unread, so `turn.started` never reaches it. What that path needs is
  a client-supplied submission id the server echoes back, which is **#1592**.
  A turn-outcome read cannot help a caller that cannot name a turn.
- **Using `user_committed` to stop re-probing the commit window.**
  The field is returned and documented, but no client path reads it yet. The
  chase that re-probes (`followSuccessor`) is deciding between *two* turns, so
  it needs to identify the successor before a per-turn read helps — again
  #1592's territory. Shipping the field now keeps the wire shape stable for
  that work rather than making it a second protocol change.
- **A failed turn on a plain page load.** Recovery paths hold a turn id; a
  cold `loadConversation` does not, so a conversation whose last turn failed
  pre-answer still renders as a question with no answer until something asks.
  Threading turn provenance through the history read is a larger change than
  this one and was left out on purpose.
- **No retention change.** A swept turn (`SweepTurnEvents`) answers 404, which
  the client reads as `unknown` and falls back. The recovery window this serves
  is minutes; the sweep's is far longer.

## Tests

- `internal/httpapi/turn_outcome_test.go` — the shaping rules as a table (no
  database: these are what a client's verdict is built from), plus DSN-gated
  end-to-end coverage over the real ledger for a turn that failed before
  replying, a running turn, owner scoping, an unknown turn and a wrong method.
- `web/src/app/chat/ui/history.test.ts` — `applyTurnOutcome` in isolation.
- `web/src/app/chat/ui/useTurnStream.reattachRecovery.test.ts` — the recovered
  model-picker banner, a replay-gap slot that now gets a verdict, a `running`
  answer that settles nothing, and a 404 that leaves the pre-#1593 behavior
  exactly as it was.
