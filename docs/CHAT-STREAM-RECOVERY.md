# Chat stream recovery — losing the socket is not losing the turn

How the `/chat` client handles an SSE stream that stops telling it what is
happening. Two rules:

- **An unknown outcome is reconciled against Postgres, never guessed.**
- **A socket that stops delivering is replaced, not mourned** — if the turn is
  still running, the stream resumes where it left off.

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
   on *looking* attached long after its socket is gone — so every recovery path
   that short-circuits on "already attached" does nothing, whether the turn has
   finished or is still running.

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

`useTurnStream.ts` implements the two rules with five pieces:

| piece | role |
| --- | --- |
| `persistedAnswersLocalTurn` (exported, pure) | Does the canonical transcript already answer the turn we're holding open — i.e. would a refresh show the reply? |
| `reconcileFromPersisted` | Programmatically does what the refresh does: adopt the canonical transcript. |
| `settleStreamedSlot` | The single finalizer every path now calls instead of stamping `done`. |
| `checkStreamLiveness` | Is the attached socket actually alive? Adopt, reconnect, or leave alone. |
| `sweepStreamLiveness` | Run that check across every attached conversation, not just the visible one. |
| `supersedeStream` | Retire a dead socket in favour of a replacement without its teardown ending the turn. |

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

## `checkStreamLiveness` — the zombie socket

The visibility/focus listener in `chat-experience.tsx` used to bail out the
moment a conversation looked attached. Under a locked phone that is exactly
when it should *not* bail. **"Attached" is not the same as "alive":** the fetch
reader of a severed socket frequently neither delivers another chunk nor
rejects, so the conversation goes on looking attached long after its
connection is gone, and nothing notices until the multi-minute idle timeout
trips.

Two different things can be true behind that silence, and they need opposite
responses:

- **the turn finished while we were away** → there is an answer in Postgres.
  Adopt it (`reconcileFromPersisted`), retire the socket, idle the composer.
- **the turn is still generating** → there is nothing to adopt yet. Replace the
  dead socket and resume the live stream from our last applied event id, so
  tokens start landing again instead of the user watching an indicator that
  will never move.

`checkStreamLiveness(convId, { force })` is the single entry point for both. It
returns `"idle" | "healthy" | "recovered" | "reconnected"`.

### Proving a socket is dead

The whole difficulty is telling a **dead** socket from a merely **quiet** one.
Guessing wrong in the timid direction leaves the user stuck; guessing wrong in
the eager direction tears down a working stream.

Suspicion comes from either of two independent kinds of evidence. Confirmation
always comes from the same grace window.

**Evidence A — the server has emitted past us.** `/inflight` reports
`last_event_id`; compare it against the client's applied
`lastEventIdByConvRef`. If the server is ahead, events exist that we are not
receiving.

**Evidence B — the promised keepalives stopped arriving.** Evidence A alone has
a blind spot, and it is a big one: while the agent sits in a long tool call the
server emits *nothing*, so it can never get ahead of us, and a socket that dies
in that stretch is invisible. The keepalive does not care what the turn is
doing — an attached stream writes at least one byte every
`FLEET_SSE_HEARTBEAT_INTERVAL` (a real event resets the timer, otherwise a
`: keepalive` comment fires). Miss several in a row and the connection is gone,
whatever the turn is doing.

The client knows the cadence because **the server advertises it**, on both
halves of the existing discovery surface: the
`X-Fleet-Heartbeat-Interval-Ms` response header (what a `fetch` client reads)
and a `heartbeat_ms` field on the synthetic first `fleet.capabilities` frame
(what a header-blind `EventSource` would get). Guessing a cadence would be
wrong in both directions — too short and every healthy stream on a slow
deployment gets killed, too long and the blind spot stays open. **Keepalives
disabled is advertised as `0`, and the client then treats silence as proving
nothing** and falls back to Evidence A alone; assuming a cadence that will
never arrive would eventually declare every healthy stream dead.

The threshold is four intervals (a full minute at the 15s default),
deliberately generous: three consecutive keepalives can be lost to a GC pause
or a slow paint without the socket being dead, and being late costs the user a
few seconds while being early costs a needless reconnect.

**Confirmation — our socket then produces nothing.** Wait
`streamLivenessGraceMs` (2.5s) and check the stream's *pulse*. A socket that
was merely frozen — a backgrounded tab, a suspended process — flushes its
buffered bytes as soon as the page thaws, well inside that window. A severed
one delivers nothing, ever.

The pulse (`streamPulseRef`) is stamped by the stream pump on **every** chunk,
including a heartbeat comment. It records both a timestamp (used by the
silence gate below, and by Evidence B) and a monotonic counter (used for the
grace-window comparison, so a socket that delivers and then goes quiet again
still counts as alive).

**A false positive is cheap by construction.** The replacement attaches with
`Last-Event-ID` set to what we already applied, the server replays from there,
and `stepStreamDedup` drops anything already seen. The cost of being wrong is
one reconnect — never a duplicated or a lost token.

### Retiring a stream without tearing down its turn

Aborting the old socket is the subtle part. Both stream owners have teardown
that assumes an ending socket means an ending turn:

- `submitPrompt`'s catch reads `signal.aborted` as **the user pressed Stop**
  and marks the turn cancelled;
- both its `finally` and `reattachToConv`'s release the attach handle, idle the
  composer, and `settleStreamedSlot` the assistant slot — which, with a turn
  still running and nothing yet in Postgres, stamps a "connection dropped"
  failure and forces the replacement onto a second, duplicate bubble.

`supersedeStream` marks the specific controller in `supersededStreamsRef` (a
`WeakSet` keyed on the controller object — no rename migration, no leak) before
aborting it. Every teardown path consults that marker and, when set, touches
nothing but its own controller entry: the handles and the slot belong to the
replacement now. It then waits (bounded) for the retiring stream's
`reattachInFlightRef` guard to clear, or the replacement would refuse to
attach and the conversation would end up with no stream at all.

### When the check runs, and over what

`sweepStreamLiveness` runs `checkStreamLiveness` over **every attached
conversation**, not just the one on screen. Chats stream in parallel — the
sidebar paints a working dot per busy chat — and a background conversation's
socket dies exactly the same way the foreground one's does; it just has nobody
watching it. The attached set is snapshotted first (a reconnect mutates it) and
per-conversation failures are contained, so one bad conversation cannot abort
the sweep.

It is driven from two places:

- **Tab return** (`visibilitychange` / `focus` / `online`) with `force: true` —
  an explicit return is always worth a probe.
- **A watchdog interval** (`STREAM_WATCHDOG_INTERVAL_MS`, 10s). A phone that
  unlocks straight back into the chat may produce exactly one visibility event,
  before the turn it was watching has finished; without a recurring check, a
  socket that dies while the tab stays open is only noticed by the idle
  timeout.

Both no-op while the tab is hidden: a hidden tab's timers are throttled and its
sockets may be legitimately frozen, so probing then would be unreliable and
pointless. The tab-return listener covers the wake-up.

The sweep is nearly free on a healthy stream. `checkStreamLiveness` carries a
**silence gate** sized from the advertised cadence (two intervals, floor 5s;
20s when no cadence is advertised), so an unforced tick on a stream that has
produced bytes within one keepalive reads a ref and returns without spending a
request. Only genuine silence buys a probe. A *forced* call still skips a
socket that delivered inside the grace window it would otherwise sit through —
on desktop `focus` fires often, and those probes could only ever conclude
"healthy". Overlapping runs are self-guarded per conversation, so a tick
landing on top of a tab return is a no-op.

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

## Reattaching to a turn that is already on screen

`reattachToConv` declines when the conversation already ends in a **completed**
assistant message and the turn it is being sent at is one whose events we have
already applied. Without that, a reattach finds no live slot to reuse and
appends a fresh `thinking` one; every replayed event is then dropped by the
Last-Event-ID dedup, because we applied them the first time. Nothing lands in
the slot and no terminal event clears it — a spinner under a finished answer.

Two snapshots can send the client back at such a turn, and only the first was
originally guarded:

- **`inflight: false` with a `turn_id`** — the server is holding the finished
  turn's retain buffer. Once `loadConversation` pulled the persisted shape the
  buffer is redundant (PR #94).
- **`inflight: true` for a turn we already streamed** — the lagging snapshot.
  `followQueueDrain` re-reads the queue milliseconds after a turn ends, and
  both that read and `/inflight` can still describe it as running, so the
  follower is sent back at a turn that is already rendered.

The second case is why the guard tests the turn id rather than only the
`inflight` flag: it declines when `currentTurnIdByConvRef` already matches the
probed `turn_id` *and* at least one event has been applied for it. That cannot
swallow a genuinely new turn (different id), nor one not yet started (no events
applied), nor a resume after a dropped socket (the slot is still `thinking` or
`streaming`, so it is reused rather than re-created).

`settleStreamedSlot` does erase such a slot on the way out — which is why this
presented as an *intermittent* orphan rather than a reliable one. It stops
erasing it when another attach is in flight concurrently, because
`loadConversation` short-circuits while the conversation still looks attached.
Declining the reattach removes the slot at the source instead of depending on
the cleanup winning that race.

## Tests

- `useTurnStream.reconcile.test.ts` — `persistedAnswersLocalTurn`, including
  the stale-transcript refusals.
- `useTurnStream.reattachRecovery.test.ts` — drives `reattachToConv` against a
  severed stream, a truncated (clean-EOF, no terminal event) stream, and a
  healthy one; the pending-approval case; and `checkStreamLiveness` across all
  four of its outcomes, including a full reconnect over both a reattach socket
  and a live `POST /chat` socket (asserting the resume point, the reused slot,
  the preserved partial answer, and the absence of any stray
  cancelled/failed marker from the retired stream).

  Each guard was mutation-checked: removing the superseded marker from either
  teardown path, the grace-window check, either evidence test, the silence
  gate, the retire identity check, the disabled-keepalive fallback, the sweep's
  visibility gate, its error containment, or its coverage of background
  conversations each fails exactly one test. The harness deps object is typed
  as `TurnStreamDeps` rather than cast through `unknown`, so adding a
  dependency is a compile error there instead of an `undefined` at runtime.

- `useTurnStream.queueDrain.test.ts` — the follow-the-drain suite, including
  *"does not re-stream a turn already on screen when the queue snapshot lags"*,
  which pins the guard above. It asserts on the **number of streams opened**
  rather than on the leftover slot: opening the second stream is the defect,
  and the `settleStreamedSlot` cleanup that usually hides it is not part of the
  contract. Asserting the slot instead reproduces only under concurrency, which
  is what made this a ~5% CI flake instead of a failing test.

- Go: `capabilities_test.go` covers the advertised cadence in both the header
  and the `fleet.capabilities` frame, and that keepalives-off advertises `0`.

## Detection latency

With the default 15s keepalive, a dead socket on a visible tab is caught within
roughly **a minute**: four missed keepalives (60s) plus the 2.5s grace window,
found by a watchdog tick at most 10s later. A dead socket discovered on tab
return is caught in the 2.5s grace window alone. Both replace the five-minute
`streamIdleTimeoutMs`, which is now only a backstop.

## Honest scope

- **The idle timeout is unchanged** (`streamIdleTimeoutMs`, 5 minutes). It is
  no longer the primary recovery path but is kept as the backstop for the one
  case liveness detection deliberately cannot cover.
- **With keepalives disabled** (`FLEET_SSE_HEARTBEAT_INTERVAL=0`), Evidence B
  is unavailable by construction and detection falls back to Evidence A alone.
  A socket that dies during a quiet stretch on such a deployment is caught by
  the idle timeout, as before. This is a deliberate consequence of not
  assuming a cadence the server has told us it will not send — the fix, if a
  deployment wants it, is to leave keepalives on.
- **Detection is foreground-only.** Both the watchdog and the tab-return sweep
  no-op while the tab is hidden, because a hidden tab's timers are throttled
  and its sockets may be legitimately frozen. Backgrounded *conversations* are
  covered (see the sweep); a backgrounded *tab* is not, and does not need to
  be — nothing is on screen to be wrong, and the wake-up is handled on return.
- **Server-side change is limited to discovery.** The buffer, the retain
  window, the keepalive itself and the persistence ledger are untouched; the
  only addition is advertising the keepalive cadence the server was already
  sending, so the client can reason about silence instead of guessing.
