# `fleet acp` — fleet as an Agent Client Protocol agent

`fleet acp` makes fleet speak the **Agent Client Protocol**
([agentclientprotocol.com](https://agentclientprotocol.com) — the Zed/JetBrains
protocol, not IBM's older Agent Communication Protocol that merged into A2A)
(#984). An ACP client launches `fleet acp` as a subprocess and drives it with
JSON-RPC over stdio. Each prompt becomes one governed fleet turn, and the reply
streams back. Buzz (via `buzz-acp`), Zed, JetBrains and any other ACP client
can use it the same way.

## Shape: a translation, not a new seam

```
ACP client ──JSON-RPC stdio──► fleet acp ──POST /chat (loopback)──► fleet serve ──► agentcore.Run
(buzz-acp, Zed, …)             (ACP agent)                          (running daemon)   (one governed loop)
```

`fleet acp` never runs an agent in-process. Each `session/prompt` is one
`POST /chat` turn against the **running** fleet server, which is exactly what
`fleet chat` sends. So every ACP turn still gets the same sandbox, cost/token
ceilings, audit trail, approval cards and host-side MCP credential broker as a
web chat. This is the A2A doctrine (ADR-0051, `docs/A2A.md`) applied to a
session-shaped protocol: one caller in, one governed run, one outcome out, and
no second loop (ADR-0001). `TestPackageStaysAClient` fails if `internal/acp`
ever imports an execution package (`agentcore`, `agent`, `sandbox`, `tools`,
`mcp`, `creds`, `store`, …).

A2A and ACP are complementary. A2A is HTTP, task-shaped, agent-to-agent, and
lands on the scheduled-task seam. ACP is stdio, session-shaped, client-to-agent,
and lands on the chat seam.

## Spec pin

This adapter speaks **ACP protocol version 1**, pinned in one place:
`internal/acp.SpecVersion`, which is the generated `ProtocolVersionNumber` of the
SDK below. Bumping the SDK is a deliberate PR that re-checks the mapping in
`internal/acp` against the new schema.

Wire types and JSON-RPC framing come from
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk) v0.13.5,
which is generated from the official ACP schema and imports only the standard
library. Only its types and its connection are used. fleet is the executor.

## Using it

On the box running fleet, identity resolves exactly as it does for `fleet chat`:

```sh
fleet acp --email acp-bot@example.com
```

The shared server token is read from the server's env file (`$FLEET_ENV_FILE`,
else `.env.local`, else `/etc/fleet/fleet.env`) or from `$FLEET_SERVER_TOKEN`
or `--token-file`. It is never accepted on argv. The email is the fleet user
every ACP turn runs as, so it is the identity in the audit trail. Provision a
dedicated bot user for it (`fleet chat user add acp-bot@example.com --password -`).

For Buzz, point `buzz-acp` at it:

```sh
export BUZZ_ACP_AGENT_COMMAND=fleet
export BUZZ_ACP_AGENT_ARGS=acp
export FLEET_USER_EMAIL=acp-bot@example.com
# plus the usual BUZZ_PRIVATE_KEY / BUZZ_RELAY_URL
buzz-acp
```

Flags: `--email`, `--server`, `--token-file`, `--env-file`, `--model`
(the model a new session's conversation starts on; the workspace default otherwise. It is never re-sent on later prompts, so a model switch made in the web UI sticks), `--persona`, `--public-url`
(the web UI base for links; when `--server` or `$FLEET_CHAT_URL` picks the
server, only this flag is trusted, since an ambient public URL may belong to
another deployment), and `--timeout` (default 30m, `0` = no bound). stdout carries protocol frames only, and every
diagnostic goes to stderr.

## Protocol mapping

| ACP | fleet |
| --- | --- |
| `initialize` | Protocol 1; `agentInfo` `fleet` + the build version; capabilities: text prompts, embedded text resources (`promptCapabilities.embeddedContext`), and `loadSession: false`. No auth methods, no image or audio. |
| `session/new` | Records a session. The fleet conversation is created by the first prompt, like a new web chat. Client-supplied `mcpServers` are **refused** (invalid params), not ignored: fleet's connectors come from the operator's bundle and run host-side with brokered credentials. |
| `session/prompt` | One `POST /chat` turn. Later prompts in the session continue the same fleet conversation. The conversation id is taken from the `X-Fleet-Conversation-Id` response header as well as the first frame, so a stream that dies early does not start a second conversation on retry. Each prompt carries an idempotency `input_id`: the client's `messageId` when it sends one (echoed back as `userMessageId`, and scoped to the ACP session, since clients may number messages per session), otherwise a key that a retry of the same text reuses after a lost answer. fleet records that key whether the prompt started a turn directly or was queued (`docs/INPUT-QUEUE.md`), so a resend is answered with the original input and never run twice. The reply then says the message is already running, already ran, or is still queued, and where to follow it. Each prompt with an unknown outcome (a lost answer, or a 5xx, which can follow an input fleet committed), or whose cancel fleet did not confirm, keeps its own key until it is answered, so unrelated prompts in between do not break a later retry. A prompt whose answer was lost while it was being cancelled is stopped by its key (`POST /conversations/{id}/cancel` with `input_id`): fleet withdraws it if still queued, cancels its turn if running, and refuses to launch it if its turn has not registered yet, atomically with registration; if the prompt has not reached fleet at all, fleet takes its key with a cancelled record, so the late arrival never runs. A Stop fleet does not accept is reported as unconfirmed, and one that reached fleet after the turn had already finished is read to the turn's end and says so: the prompt still ends `cancelled`, as ACP requires, with a note that nothing was stopped and what the turn did stands. A retry of an unresolved prompt goes to the conversation it was first sent to (none, for a session's first prompt), since fleet recognises a key only there; it does not move the session to another conversation. If fleet reports that an earlier attempt was accepted but never ran, the prompt is resubmitted under a fresh key, unless it was cancelled in the meantime. For a `messageId` the fresh keys are derived from it (`<key>-r1`, `<key>-r2`, …, at most 8), so a later resend of the same `messageId` walks the same chain to the attempt that ran, however long ago, with nothing held in memory; fleet looks these keys up per user, so the walk finds them in whichever conversation accepted them. A text-only prompt gets one random fresh key. A `messageId` whose whole chain was accepted and never ran fails asking for a new message, since a resend would walk the same chain. A prompt cancelled while fleet was queueing it, or whose resend was answered with an earlier attempt that is still queued or running, is stopped by its key the same way. If the conversation already has a running turn (started from another surface, such as the web chat), fleet queues the prompt to run after it; the prompt then ends `end_turn` with a note saying it was queued and where to follow it, rather than an error inviting a retry. The response carries `_meta["fleet.conversationId"]` and token `usage`. |
| `session/cancel` | Stops the turn **server-side** (`POST /conversations/{id}/cancel`, scope `turn`) and answers the prompt with stop reason `cancelled`. Closing the HTTP stream alone would not stop the turn, because fleet detaches a turn from its request by design. If fleet does not accept the Stop, the prompt still answers `cancelled` (ACP requires it), but the transcript says the turn may still be running and where to stop it. A turn stopped from another fleet surface, such as the web chat's Stop, also ends as `cancelled`, including a Stop that cancelled the prompt before its turn started. The Stop names the watched turn (`turn_id` from `turn.started`), and the server cancels it only while it is the running turn, so it can never hit a follow-up queued from another surface. The turn id comes from the `X-Fleet-Turn-Id` response header or the `turn.started` frame. A turn already reported as over is never sent a Stop, and an untargeted Stop is never sent: without the turn id the stop is reported as unconfirmed instead. A prompt cancelled before it was submitted (while waiting for an earlier prompt on the session) is never sent to fleet. |
| `session/close` | Forgets the session. The conversation stays in fleet like any chat. |
| `authenticate`, `logout`, `session/load`, `session/list`, `session/resume`, `session/set_mode`, `session/set_config_option` | Not advertised. They answer method-not-found. |

Stream events map onto `session/update`:

| fleet SSE (`POST /chat`) | ACP `session/update` |
| --- | --- |
| `text.delta` | `agent_message_chunk` |
| `reasoning.delta` | `agent_thought_chunk` |
| `tool.call` / `tool.result` | `tool_call` (title = tool name, `in_progress`) / `tool_call_update` (`completed` or `failed`; `pending` for a call that fleet staged as an approval card, whose placeholder result is not a failure; a call whose staging itself failed stays `failed`) |
| `text.replace` | Nothing when it matches what was streamed; the missing suffix when it extends it; otherwise the final text after a `— revised answer —` line (see below) |
| `tool.approval_required` | When the turn ends, however it ends (completed, cancelled, timed out or errored), a text pointer to the approval in fleet. A `preview_email` card is display-only (its one action is Dismiss), so it gets a "draft preview is open, nothing was sent" pointer, never approve instructions, and its tool call shows `completed`. |
| `turn.policy_blocked` | Stop reason `refusal` |

Prompt content: text blocks are joined. A `resource_link` becomes a Markdown
link. An embedded text resource is inlined under its URI. Images, audio and
binary blobs are refused with invalid params, which matches what `initialize`
advertises.

## Errors

| Failure | What the ACP client sees |
| --- | --- |
| No email / no token configured | `initialize` still succeeds; `session/new` answers `auth_required` (-32000) with the same fix-it text `fleet chat` prints. `fleet acp` keeps running, so the client shows the reason instead of "agent exited". |
| Wrong token (403) / user not authorized (401) | `auth_required`: the 403 text names `FLEET_SERVER_TOKEN`, and the 401 text names the user. The token value is never included. |
| Daemon down | Internal error (-32603) naming the unreachable server URL |
| `turn.error` / `turn.model_required` | Internal error carrying the server's message |
| `--timeout` exceeded | The turn is stopped server-side, then an internal error names the timeout and the flag. If the Stop fails, the error says so and that the turn may still be running. |
| Unknown session id | -32002 resource not found |

## Honest scope

What shipped:

- `fleet acp`, an in-tree stdio ACP agent in the one `fleet` binary, covering
  `initialize`, `session/new`, streaming `session/prompt`, `session/cancel` and
  `session/close`.
- Turns go through the running server's governed chat seam and are visible in
  the web UI as ordinary conversations owned by the configured user.
- Tests in `internal/acp` drive a real SDK client over pipes against a fake
  server that speaks the `POST /chat` SSE contract. They cover streaming, the
  follow-up turn, `text.replace` reconciliation, cancel, timeout, 403,
  daemon-down, `turn.error`, policy refusal, approval pointers, refused
  content, the stdio entry point (stdout carries only JSON-RPC), and the
  no-execution-imports guard. No live model, no live Buzz in CI.
- Checked by hand against a real `fleet serve` (mock mode, local Postgres):
  multi-turn sessions persisted to one conversation, the approval pointer, 403,
  daemon-down, and missing email. The Stop endpoint was checked with a
  hand-made request of the same shape (token, email, `{"scope":"turn"}`),
  because a mock turn finishes too fast to cancel mid-flight.

Deviations and limits:


- **Approvals stay in fleet.** A turn that stages an approval card ends with a
  pointer: `FLEET_PUBLIC_BASE_URL` (or `FLEET_PUBLIC_URL`) + `/chat?c=<id>`,
  or, when neither is set, the `fleet chat --conversation … --approve …`
  command. The public URL must describe the deployment the turn ran on:
  `--public-url` always wins; with `--server` / `$FLEET_CHAT_URL` nothing
  else is trusted; when an env file supplied the token or address, only that
  file's public URL is used; otherwise the environment, then a pinned env
  file. It is never read from a different deployment's file. ACP's
  `session/request_permission` is deliberately not used to re-implement the
  default-deny card.
- **Streamed text is append-only.** ACP cannot retract a chunk. When an
  enforcement round replaces a draft that was already streamed, the client
  gets the final answer again after a `— revised answer —` line, so it ends
  on what fleet persisted.
- **Tool detail stays in the run log.** Tool calls appear as titled
  `tool_call` updates with a status. Inputs and outputs are not forwarded.
- **The client's filesystem and terminal are not used.** Tool calls run in
  fleet's sandbox workspace. The session `cwd` is recorded, not mounted.
- **No `session/load`.** A session lives as long as the `fleet acp` process.
  The conversation itself persists in fleet, but resuming it over ACP is
  deferred until the session id can round-trip honestly.
- **Stop reasons are `end_turn`, `cancelled` or `refusal`.** A fleet ceiling
  (cost, tokens, iterations) ends the turn through fleet's normal path, and the
  adapter does not guess `max_tokens` / `max_turn_requests` from it.
- **One identity per process.** Every turn runs as the configured fleet user.
  Per-Buzz-user mapping is out of scope.

Deferred: a Buzz Desktop catalog entry (a custom command works today),
`session/load`, and an ACP *client* in fleet (launching other ACP agents is a
different feature).
