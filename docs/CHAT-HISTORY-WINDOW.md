# Opening a long conversation: the history window

Opening a conversation used to download its whole history. For a long
Workbench or Patrol thread that is megabytes of tool output the reader will
mostly never scroll back to, and they waited for all of it before the first
message was drawn. A conversation now opens on its newest turns, and **Load
earlier messages** pages back.

User-facing behaviour is in the chat guide
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`, "Long
conversations"), which `/help` renders.

## What was slow

1. **The read was unbounded.** `GET /conversations/{id}` called
   `store.LoadHistory`, which returns every stored entry with its full content:
   each tool result up to 64 KiB (128 KiB hard cap), every tool-call input,
   every reasoning block. A synthetic thread of 150 turns with four 20 KB tool
   results each (1,830 entries) answers with **13.1 MB** of JSON. Nothing
   bounded it and no caller could ask for less.
2. **The browser then did work proportional to all of it** before first paint:
   parse the JSON, rebuild every message (`historyToMessages`, which also
   parses every `run_python` result), and derive card state over the lot.
3. **One refresh re-downloaded the whole history by accident.** The
   Autonomous auto-submit refresh asked for
   `?omit_history=1&approval_index=1`, but the gateway forwarded no query on
   this route, so each refresh was a full read.
4. **Compression depended on the proxy.** The single-box Caddyfile encodes
   responses; the Helm chart's Ingress and a bare `fleet serve` sent the JSON
   as it was.

Not the problem: the queries. The conversation read makes about a dozen small
indexed queries with no N+1 among them, and the history itself is one index
range scan on `(conversation_id, id)`. The time went to moving and parsing the
payload, not to finding it.

## What shipped

### The windowed read (opt-in)

```
GET /conversations/{id}?limit=N              the newest window
GET /conversations/{id}?limit=N&before=ID    the page older than ID
GET /conversations/{id}?from=ID              everything from ID's turn onward
```

A request that names none of these gets the whole history and exactly the
response it always got: no new key, no changed value. `limit` is clamped to
500; a malformed parameter is a 400. The browser gateway forwards
`omit_history`, `limit`, `before` and `from` on this route and nothing else
(`approval_id` and the terminal's review flags stay off it).

A windowed response adds one key:

```json
"history_page": {
  "has_more": true,
  "next_before": 1234,
  "earlier": {
    "cards": [ ... ],
    "totals": {"cost_usd": 1.5, "prompt_tokens": 600, "cached_tokens": 300, "turns": 6},
    "summary_id": 77
  }
}
```

`next_before` is the id the window starts at: pass it as `before` for the next
older page, or as `from` to re-read the same window. `earlier` is present while
`has_more` is true.

**A window only ever begins where a turn opens.** `limit` is a minimum: the
window is widened back to the opening message of the turn the limit falls in
(a user message that was not steered into a running turn), and `from` is moved
back the same way. This is what makes a window safe, not just tidy. What a
participant may see of an entry depends on who sent the turn it belongs to
(`historyForParticipant`), and a window that began partway through a
teammate's turn would be read as the owner's. Beginning at the opening message
leaves nothing to carry in.

One case remains where a window alone cannot name a sender: a tool result
written turns after its call with no `sender_email` (startup recovery settling
an interrupted approval). When a window holds such a result and its call is
older than the window, the response is projected from the whole history
instead (`windowNeedsWholeHistory`). It is rare, it costs what the read cost
before, and it is never a guess. `TestHistoryWindowProjectsLikeTheWholeHistory`
checks every window of a multi-sender conversation, for each participant,
against the whole-history projection.

The search for a turn's opening message reads ids, roles and types, and the
content of user messages only, in chunks from the newest entry back
(`store.historyTurnStart`), so finding a window never reads a tool result.

### What a window leaves out: `earlier`

Some views are built from the whole conversation. A client holding a window
gets what the unloaded part contributes to them, projected for the viewer by
the same `historyForParticipant`:

- **`cards`**: every `show_ui` call that drew a card, with its result, and the
  user messages that answered a card. Dashboard view builds its panels from
  these, so a panel drawn long before the window is on the board on first
  load, at the version, position and answer the whole conversation gives it.
- **`totals`**: the cost and tokens of the turns not loaded, summed with the
  web's own rules, so the totals chip reads the same either way.
- **`summary_id`**: where the compaction summary sits, when it is older than
  the window and the viewer is shown it, so panels older than it read as
  retired.

It comes from `store.LoadHistoryOutline`: one query for the user messages,
turn summaries, compaction markers and `show_ui` rows older than the window.
No tool output, reasoning or reply text is read into the response.

The card entries are bounded at 512 KiB. Past that the oldest cards that
something later replaced are dropped, call and result together, and
`cards_trimmed` is set. A card nothing has replaced is never dropped.

### The web client

- **First open asks for the newest window** (`?limit=80`,
  `HISTORY_WINDOW_LIMIT`). **Load earlier messages** is the first row of the
  transcript while older turns exist. It is a button (`aria-busy` and a polite
  status while its page loads; a failed page says so and the button offers it
  again). The page is put in front of the loaded messages, which keep their
  identity, so nothing on screen remounts.
- **The reader's place is kept.** The transcript is virtualized and opts out
  of browser scroll anchoring, so the scroll offset is moved by exactly the
  height that arrived above. If the button held the keyboard focus, the focus
  goes to the transcript when the button moves away with its page.
- **A reload never shrinks what is loaded.** A navigation starts over with the
  newest window. Every other load (returning to the tab, the background
  revalidation, recovery after a dropped stream) re-reads `?from=` where the
  held part starts. The recovery checks compare the server's history with the
  local messages turn for turn, so they read the same range.
- **A page that arrives late is dropped** if the held window moved while it
  was in flight, and a turn already loaded is never added twice.
- **Approval cards.** A settled card whose tool call is older than the loaded
  part waits for the page that holds its call instead of piling onto the last
  reply. A card still waiting on the reader, or still running, is always
  shown.
- **A server that names no page** (an older one) is treated as having
  returned the whole conversation, as before.

Two small render costs went with it: the task-tracker list a reply shows is
parsed once per tool call rather than on every render, and a collapsed Python
output counts its lines without splitting them.

### Compression

The gateway gzips the JSON reads of the chat route table for a browser that
accepts it (`internal/webui/gzip.go`): GET rows whose response is JSON and
carries no validator, a 200, and at least 1 KiB. Streams, downloads, uploads
and the ETag-carrying passthrough rows are untouched. A proxy that encodes
leaves an already-encoded response alone, so behind Caddy nothing is
compressed twice.

## Measured

`BenchmarkConversationGet` (`internal/httpapi`), the 150-turn thread above,
handler time against a local Postgres:

| Read | Response | Handler time |
| --- | --- | --- |
| Whole history (before, and still without `limit`) | 13.1 MB | ~83 ms |
| `?limit=80`, the web client's first open | 0.62 MB | ~19 ms |

The response is about 21 times smaller. The handler time is a floor, not the
user's wait: what the browser saves is the transfer and the parse of the other
12.5 MB, which were not measured here.

## Deviations and what was left

- **The window is entries, not bytes.** A turn is never split, so one turn
  with many large results is one page however large it is.
- **Tool results are returned whole.** The live stream shows a result's first
  4,000 bytes while the reload returns all of it; bounding the reload the same
  way would shrink a page further but changes what an expanded tool chip shows
  after a reload, so it was left for its own change.
- **Resolved approval cards are still listed whole** on every read, not only
  the ones in the window.
- **A trimmed board loses version counts.** When `cards_trimmed` is set, a
  panel whose early versions were dropped shows a lower `v` number than the
  whole history would.
- **Message rows are not individually memoized.** The transcript is
  virtualized and its Markdown is memoized; the row body is still inline JSX
  that re-renders for mounted rows on each streamed delta.
- **The boot sequence is unchanged.** Session, then the conversation list,
  then the conversation still run in turn; the list decides which
  conversation opens.
- **Recovery still reads the conversation twice** when it adopts a persisted
  answer (once to decide, once to load).
- **`fleet chat` (the terminal client) and the public share and team views**
  read the whole history as before.

## Tests

Go: `internal/store/history_window_test.go` (the cut at a turn's opening
message, paging without gaps or repeats, steered messages, scoping, `from`,
the outline); `internal/httpapi/history_window_test.go` (a read with no window
is unchanged, paging back to the whole history byte for byte, bad parameters,
the access gate, the per-participant projection for every window, earlier
cards and totals for the owner and a teammate, the bound);
`internal/webui/gzip_test.go` and `routes_test.go` (compression, the forwarded
parameters).

Web: `historyWindow.test.ts` (the page parser, the read query, prepend and
dedupe, approval deferral, the board and card answers from earlier cards);
`chat-experience.historyWindow.test.tsx` (the windowed first load, **Load
earlier messages** to the first message, overlap, busy and failed states, a
page that arrives late, another chat, the `from` re-read, scroll and focus,
dashboard panels); `transcriptRows.test.ts` (the row).
