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
(new sessions; the workspace default otherwise), `--persona`, and `--timeout`
(default 30m, `0` = no bound). stdout carries protocol frames only, and every
diagnostic goes to stderr.

## Protocol mapping

| ACP | fleet |
| --- | --- |
| `initialize` | Protocol 1; `agentInfo` `fleet` + the build version; capabilities: text prompts, embedded text resources (`promptCapabilities.embeddedContext`), and `loadSession: false`. No auth methods, no image or audio. |
| `session/new` | Records a session. The fleet conversation is created by the first prompt, like a new web chat. Client-supplied `mcpServers` are **refused** (invalid params), not ignored: fleet's connectors come from the operator's bundle and run host-side with brokered credentials. |
| `session/prompt` | One `POST /chat` turn. Later prompts in the session continue the same fleet conversation. The conversation id is taken from the `X-Fleet-Conversation-Id` response header as well as the first frame, so a stream that dies early does not start a second conversation on retry. The response carries `_meta["fleet.conversationId"]` and token `usage`. |
| `session/cancel` | Stops the turn **server-side** (`POST /conversations/{id}/cancel`, scope `turn`) and answers the prompt with stop reason `cancelled`. Closing the HTTP stream alone would not stop the turn, because fleet detaches a turn from its request by design. If fleet does not accept the Stop, the prompt still answers `cancelled` (ACP requires it), but the transcript says the turn may still be running and where to stop it. A turn stopped from another fleet surface, such as the web chat's Stop, also ends as `cancelled`. A turn the server already reported as over is never sent a Stop: the Stop is conversation-scoped and could otherwise cancel a follow-up queued from another surface. |
| `session/close` | Forgets the session. The conversation stays in fleet like any chat. |
| `authenticate`, `logout`, `session/load`, `session/list`, `session/resume`, `session/set_mode`, `session/set_config_option` | Not advertised. They answer method-not-found. |

Stream events map onto `session/update`:

| fleet SSE (`POST /chat`) | ACP `session/update` |
| --- | --- |
| `text.delta` | `agent_message_chunk` |
| `reasoning.delta` | `agent_thought_chunk` |
| `tool.call` / `tool.result` | `tool_call` (title = tool name, `in_progress`) / `tool_call_update` (`completed` or `failed`; `pending` for a call that fleet staged as an approval card, whose placeholder result is not a failure; a call whose staging itself failed stays `failed`) |
| `text.replace` | Nothing when it matches what was streamed; the missing suffix when it extends it; otherwise the final text after a `— revised answer —` line (see below) |
| `tool.approval_required` | When the turn ends, however it ends (completed, cancelled, timed out or errored), a text pointer to the approval in fleet |
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
  command. The public URL is read from the environment or from the same env
  file that supplied the server token or address, never from a different
  one, so the link points at the deployment the turn ran on. ACP's
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
