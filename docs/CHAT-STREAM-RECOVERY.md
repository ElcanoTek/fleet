# Chat stream recovery — losing the socket is not losing the turn

How the `/chat` client settles an assistant message whose SSE stream ended
without telling it how the turn came out. The short version: **an unknown
outcome is reconciled against Postgres, never guessed.**

## The failure this exists to prevent

> I set up a long-running task on my phone and walked away. The phone locked.
> When I came back and opened the site it said the assistant hadn't replied —
> but if I refresh the page the full answer is right there, and everything
> actually worked.

Two things go wrong when a phone locks mid-turn:

1. **The socket dies while the server keeps working.** iOS/Android sever the
   TCP connection on screen-lock. The server never notices anything unusual: it
   finishes the turn, writes the transcript to Postgres, and seals the event
   buffer. The browser just stops receiving.
2. **The browser doesn't find out promptly.** A suspended page's `fetch` reader
   frequently neither delivers another chunk nor rejects. The conversation goes
   on *looking* attached long after its socket is gone.

Neither of those is the bug. The bug was what the client did next: every
finalizer in the turn loop settled the orphaned assistant slot by stamping
`state: "done"` in place. That turns "we don't know" into a **terminal
success with no content**, which the transcript renders as *"The assistant
finished without a written reply."* (or, on the live-stream path, the literal
*"No response returned."*). Both are assertions about what the model did, made
from the fact that a socket closed — and both were flatly contradicted by the
database. Refreshing worked precisely because a reload reads Postgres.

## The rule

> A finalizer may only claim an outcome it observed. Anything else asks the
> server.

`useTurnStream.ts` implements that with three pieces:

| piece | role |
| --- | --- |
| `persistedAnswersLocalTurn` (exported, pure) | Does the canonical transcript already answer the turn we're holding open — i.e. would a refresh show the reply? |
| `reconcileFromPersisted` | Programmatically does what the refresh does: adopt the canonical transcript. |
| `settleStreamedSlot` | The single finalizer every path now calls instead of stamping `done`. |

### `persistedAnswersLocalTurn` — the guard that makes adoption safe

Two conditions, both load-bearing:

- the persisted transcript ends in a **completed, non-failed assistant
  message**, and
- it covers **at least as many user turns** as the in-memory copy.

The second is what keeps recovery from adopting a *stale* transcript. A
conversation whose previous turn completed also "ends in an assistant reply";
without the user-turn count, recovery would swap the live turn's prompt out of
the transcript and pass the old answer off as the new one. Counting user
messages (not history rows) is the comparison that survives
`historyToMessages`' grouping — it merges an assistant's text/tool_call/
tool_result rows into one message but never merges user rows.

### `settleStreamedSlot` — what a lost stream becomes

Called with the slot the drained stream was writing to. It acts on two shapes:

- a slot still **mid-flight** (`thinking`/`streaming`) — the stream ended
  without a terminal event, so the outcome was never learned; and
- a slot already `done` but **empty after a replay gap** — the terminal event
  arrived, the answer did not (see *Replay gaps* below).

Order of resolution:

1. **Ask Postgres.** If `persistedAnswersLocalTurn` holds, adopt the canonical
   transcript and stop. This is the walk-away case, and it is the common one.
2. **Waiting on the user?** A slot holding a pending approval or memory
   proposal is blocked on a decision, not on the network — resolving the card
   resumes the turn. Settle it quietly; a "Turn failed / Retry" banner over a
   live action card would tell the reader to discard the very decision being
   asked for.
3. **Otherwise, say what happened.** `failed: true` plus *"The connection
   dropped before the response finished."*, keeping any partial answer that did
   arrive. This is honest and it offers Retry — as opposed to a blank bubble
   asserting the assistant said nothing.

Every finalizer routes through it: the reattach pump's `finally`, the live
`streamTurn` tail, and both the catch and the `finally` in `submitPrompt`.

### `reconcileStaleConv` — the zombie socket on tab return

The visibility/focus listener in `chat-experience.tsx` used to bail out the
moment a conversation looked attached. Under a locked phone that is exactly
when it should *not* bail: the socket is dead, the turn is over, and nothing
else will notice until the multi-minute idle timeout fires — so the user came
back to a stuck thinking indicator, and then to the bogus empty reply.

On tab return the listener now probes `/inflight` for a conversation whose
local transcript is mid-flight. If the turn is still generating, nothing
happens — the live socket (or the reattach that follows) owns it. If it is
provably over, the persisted transcript is adopted, the dead socket is torn
down, and the composer goes idle. That is the refresh, without the refresh.

## Replay gaps

A turn's server-side event buffer is a sliding window
(`FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN`, 5 MiB). A long, chatty turn can outrun
it, and a client reattaching after the window slid gets a synthetic
`reconnect` frame carrying `missed_events`.

The client now records that as `ctx.gap`. A gap means an empty assistant slot
is evidence that **we missed the answer**, not that there wasn't one — so a
`turn.completed` under a gap no longer writes "No response returned.", and the
slot is reconciled against Postgres once the stream drains.

## Retain window vs. the database

`bufferRetainTTL` (`FLEET_SSE_BUFFER_DURATION`, default 15 minutes) keeps a
finished turn's buffer around so a client returning quickly gets the missed
events replayed. Beyond it there is no buffer, and `/inflight` legitimately
reports nothing about a turn that completed perfectly. Recovery therefore does
not depend on the retain window at all: the buffer is a fast path, Postgres is
the source of truth, and the reconcile path is the one that always works.

## Tests

- `useTurnStream.reconcile.test.ts` — `persistedAnswersLocalTurn`, including
  the stale-transcript refusals.
- `useTurnStream.reattachRecovery.test.ts` — drives `reattachToConv` against a
  severed stream, a truncated (clean-EOF, no terminal event) stream, and a
  healthy one; plus `reconcileStaleConv` and the pending-approval case. The
  three recovery cases fail against the pre-fix finalizer.

## Honest scope

- **A turn that is still generating when you come back is not accelerated.**
  `reconcileStaleConv` deliberately does nothing while `/inflight` reports the
  turn alive, because there is no false-positive-free way to tell a dead
  socket from a quiet one, and tearing down a healthy stream costs more than it
  saves. Such a conversation still shows a truthful thinking indicator and
  still recovers via the existing idle timeout + reattach — it just isn't
  instant. Making that instant needs a liveness signal (e.g. comparing the
  probe's `last_event_id` against the client's applied id, with care taken that
  aborting a live socket isn't read as a user cancel) and is a separate change.
- **No server-side change.** The buffer, the retain window, the heartbeat and
  the persistence ledger are untouched; the server was already correct. This is
  entirely about what the client concludes when it stops hearing from it.
