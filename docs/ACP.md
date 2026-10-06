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
another deployment), and `--timeout` (default 30m, `0` = no bound; a negative value is refused). stdout carries protocol frames only, and every
diagnostic goes to stderr.

### Neovim (CodeCompanion.nvim)

Tested against a real `fleet serve` with a live model, from Neovim 0.12.5 and
CodeCompanion.nvim at commit `3dd1ef7` (2026-10-02). On 2026-10-04, connect,
multi-turn, streamed thinking and tool calls, cancel, `--timeout`, the approval
pointer, queueing behind a running turn, and the daemon-down and missing-email
errors all worked through CodeCompanion. The adapter below extends
CodeCompanion's `goose` preset only as a convenient base for a plain ACP
command:

```lua
require("codecompanion").setup({
  adapters = {
    acp = {
      fleet = function()
        return require("codecompanion.adapters").extend("goose", {
          name = "fleet",
          formatted_name = "fleet",
          opts = { vision = false },
          commands = {
            default = { "fleet", "acp", "--email", "acp-bot@example.com" },
          },
          defaults = {
            mcpServers = {}, -- fleet refuses client-supplied MCP servers: keep this, not "inherit_from_config"
            timeout = 20000, -- CodeCompanion's wait for initialize/session/new; prompts are bounded by `fleet acp --timeout`
          },
          handlers = {
            -- fleet runs tools in its own sandbox and never reads the client's
            -- files, so send buffer/file context as embedded resources (ACP
            -- promptCapabilities.embeddedContext) instead of a bare path.
            form_messages = function(self, messages, capabilities)
              local tags = require("codecompanion.interactions.shared.tags")
              local out = {}
              for _, msg in ipairs(messages) do
                if msg.role == self.roles.user and msg._meta and not msg._meta.sent and msg.content and msg.content ~= "" then
                  local tag = msg._meta.tag
                  local p = msg.context and msg.context.path
                  if (tag == tags.FILE or tag == tags.BUFFER) and p and p ~= "" then
                    table.insert(out, { type = "resource", resource = {
                      uri = vim.uri_from_fname(vim.fn.fnamemodify(p, ":p")), mimeType = "text/plain", text = msg.content } })
                  elseif tag ~= tags.IMAGE then
                    table.insert(out, { type = "text", text = msg.content })
                  end
                end
              end
              return out
            end,
          },
        })
      end,
    },
  },
  interactions = { chat = { adapter = "fleet" } },
})
```

The `form_messages` override is what makes file and buffer context work:
`#{buffer}`, `#{buffers}`, `/buffer` and `/file` all go through it (the live
check used `#{buffer}`). The `goose` preset advertises `clientCapabilities.fs`
read and write, and CodeCompanion's stock ACP helper sends file and buffer
context as a bare text line (`Sharing the following file as context: <path>`),
whatever the agent's `promptCapabilities.embeddedContext` says. It expects the
agent to read the path itself, from a shared filesystem or back over ACP with
`fs/read_text_file`. fleet does neither (see the limits below), so without the
override the model sees only a path and asks for the file to be pasted. The
override sends CodeCompanion's rendering of the buffer (line-numbered, in its
`<attachment>` wrapper) as an embedded `resource` block, which fleet inlines
under its URI; with it, the model answered from the buffer content. It relies
on CodeCompanion internals (the `handlers.form_messages(self, messages,
capabilities)` hook, `_meta.tag` and `_meta.sent`, and the
`codecompanion.interactions.shared.tags` module path), so recheck it after
upgrading CodeCompanion.

CodeCompanion starts `fleet acp` in Neovim's current directory, with Neovim's
environment plus the adapter's `env`, and fleet resolves the token as above,
never from argv. Because the `.env.local` fallback is relative to that
directory, which is the project you are editing, set `FLEET_ENV_FILE` to the
server env file's absolute path, either in the environment Neovim starts with
or on the adapter (`env = { FLEET_ENV_FILE = "/etc/fleet/fleet.env" }`). Never
put the token itself in `env`: Neovim configs are often published as dotfiles.
Both forms were checked live (adapter `env` on 2026-10-04, the inherited
environment on 2026-10-05).

CodeCompanion's rules feature also sends project rule files. With its default
rules settings (`autoload = "default"`), the first prompt of a new chat carries
each file of its `default` rules group that exists as an extra text block
(``Sharing `AGENTS.md`: …``): `.clinerules`, `.cursorrules`, `.goosehints`,
`.rules`, `.windsurfrules`, `.github/copilot-instructions.md`, `AGENT.md`,
`AGENTS.md`, `CLAUDE.md` and `CLAUDE.local.md` in the working directory, and
`~/.claude/CLAUDE.md`. They become part of the persisted fleet conversation
(checked live with `AGENTS.md` on 2026-10-05). CodeCompanion's `rules` settings
control this; its defaults include a `rules.opts.chat.enabled` switch, which
was not live-tested with fleet.

For an adapter bug report, attach CodeCompanion's raw JSON-RPC transcript.
CodeCompanion writes one for every `fleet acp` process it starts, at any log
level, to a file in Neovim's temp directory that is deleted when Neovim exits.
With `opts = { log_level = "INFO" }` at the top level of `setup()` (not in the
adapter's `opts`), it records that file's path as `[acp] RPC log: <path>` in
`codecompanion.log` under `stdpath("log")`. Copy the file before quitting
Neovim, and read it first: it holds every prompt and the full text of each
buffer you shared.

## Protocol mapping

| ACP | fleet |
| --- | --- |
| `initialize` | Protocol 1; `agentInfo` `fleet` + the build version; capabilities: text prompts, embedded text resources (`promptCapabilities.embeddedContext`), and `loadSession: false`. No auth methods, no image or audio. |
| `session/new` | Records a session. The fleet conversation is created by the first prompt, like a new web chat. Client-supplied `mcpServers` are **refused** (invalid params), not ignored: fleet's connectors come from the operator's bundle and run host-side with brokered credentials. |
| `session/prompt` | One `POST /chat` turn. Later prompts in the session continue the same fleet conversation. The conversation id is taken from the `X-Fleet-Conversation-Id` response header as well as the first frame, so a stream that dies early does not start a second conversation on retry. Each prompt carries an idempotency `input_id`: the client's `messageId` when it sends one (echoed back as `userMessageId`, and scoped to the ACP session, since clients may number messages per session), otherwise a key that a retry of the same text reuses after a lost answer. fleet records that key whether the prompt started a turn directly or was queued (`docs/INPUT-QUEUE.md`), so a resend within the input queue's retention window (`docs/INPUT-QUEUE.md`) is answered with the original input and never run twice. The reply then says the message is already running or has finished, was already taken (how that turn ended, its reply or an error, is in the conversation), or is still queued (with its place in line when fleet reports one), and where to follow it. Each prompt with an unknown outcome (a lost answer, or a 5xx, which can follow an input fleet committed), or whose cancel fleet did not confirm, keeps its own key until it is answered, so unrelated prompts in between do not break a later retry; these keys are never evicted (a forgotten key would let its retry run twice), so they cost one small entry per lost answer for the life of the session. A prompt whose answer was lost while it was being cancelled is stopped by its key (`POST /conversations/{id}/cancel` with `input_id`): fleet withdraws it if still queued, cancels its turn if running, and refuses to launch it if its turn has not registered yet, atomically with registration; if the prompt has not reached fleet at all, fleet takes its key with a cancelled record, so the late arrival never runs. A Stop fleet does not accept, or sends but cannot confirm in time (and the turn's stream does not confirm either), is reported as unconfirmed, and one that reached fleet after the turn had already finished (whether the Stop named the turn or the input's key, the turn's final frame arrived first, or fleet accepted the Stop but the turn completed, or failed on its own, in the same instant, which the adapter learns by reading on to the turn's final frame) says so: the prompt still ends `cancelled`, as ACP requires, with a note that nothing was stopped and what the turn did stands. A retry of an unresolved prompt goes to the conversation it was first sent to (none, for a session's first prompt), since fleet recognises a key only there; it does not move the session to another conversation. If fleet reports that an earlier attempt was accepted and cancelled, the prompt is not resubmitted: fleet does not record why an input was cancelled, and a Stop from any surface looks the same as a launch that failed, so resubmitting could run a message after its Stop succeeded. The reply says it did not run and asks for it to be sent again as a new message (a new `messageId`). A text-only prompt whose lost answer's Stop fleet confirmed drops its key, so sending the same text again runs it rather than replaying the cancelled attempt. A prompt cancelled while fleet was queueing it, or whose resend was answered with an earlier attempt that is still queued or running, is stopped by its key the same way. If the conversation already has a running turn (started from another surface, such as the web chat), fleet queues the prompt to run after it; the prompt then ends `end_turn` with a note saying it was queued, its place in line when fleet reports one (next, or how many other queued messages run first), and where to follow it, rather than an error inviting a retry. The response carries `_meta["fleet.conversationId"]` and token `usage`. |
| `session/cancel` | Stops the turn **server-side** (`POST /conversations/{id}/cancel`, scope `turn`) and answers the prompt with stop reason `cancelled`. Closing the HTTP stream alone would not stop the turn, because fleet detaches a turn from its request by design. If fleet does not accept the Stop, the prompt still answers `cancelled` (ACP requires it), but the transcript says the turn may still be running and where to stop it. A turn stopped from another fleet surface, such as the web chat's Stop, also ends as `cancelled`, including a Stop that cancelled the prompt before its turn started. The Stop names the watched turn (`turn_id` from `turn.started`), and the server cancels it only while it is the running turn, so it can never hit a follow-up queued from another surface. The turn id comes from the `X-Fleet-Turn-Id` response header or the `turn.started` frame. A turn already reported as over is never sent a Stop, and an untargeted Stop is never sent. Without the turn id, the prompt is stopped by its key instead (`input_id`, as under `session/prompt`): fleet withdraws it if still queued, cancels its turn if running, and the prompt reports a confirmed stop, an input that had already finished, or an unconfirmed stop, whichever fleet answers. Only when no conversation is known either is the stop reported as unconfirmed. A prompt cancelled before it was submitted (while waiting for an earlier prompt on the session) is never sent to fleet. |
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
link. An embedded text resource is inlined under its URI, in a code fence of
at least four backticks and longer than any run of backticks in its text, so
a fence inside the file cannot end it early. A line break in a URI is
percent-encoded, and one in a link's name becomes a space, so neither can
start Markdown of its own. Images, audio and binary blobs are refused with
invalid params, which matches what `initialize` advertises.

## Errors

| Failure | What the ACP client sees |
| --- | --- |
| No email / no token configured | `initialize` still succeeds; `session/new` answers `auth_required` (-32000) with the same fix-it text `fleet chat` prints. `fleet acp` keeps running, so the client shows the reason instead of "agent exited". |
| Refused by the server (403) | `auth_required`, with text naming the reason the server gave: a wrong shared token names `FLEET_SERVER_TOKEN`; an email that is not a provisioned fleet user (`not_a_member`) names the user and `fleet chat user add <email> --password -`; a viewer-role user (`read_only`) names the user and `fleet chat user role <email> --role member`; a client address the server's IP filter refuses names `FLEET_IP_ALLOWLIST` / `FLEET_IP_DENYLIST`, without saying which list matched (the server does not say either). Any other 403 body (a reverse proxy's page, say) is quoted as a short one-line excerpt instead of being blamed on the token. The token value is never included. |
| Any 401 | `auth_required`, with text naming the user. fleet itself sends a 401 here only for a revoked web session, which `fleet acp` never presents, so in practice it comes from a proxy in front of fleet. |
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
- Checked live against a real `fleet serve` with a live model from Neovim
  0.12.5 and CodeCompanion.nvim (`3dd1ef7`): the features listed under "Neovim
  (CodeCompanion.nvim)", including buffer context sent as an embedded resource,
  on 2026-10-04; the snippet as published there and the rules-file behaviour on
  2026-10-05.

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
  fleet's sandbox workspace. The session `cwd` is recorded, not mounted. A
  client that shares a file by sending only its path, rather than an embedded
  resource, loses that context: fleet cannot read the path. CodeCompanion.nvim
  does this by default; the configuration under "Neovim (CodeCompanion.nvim)"
  works around it.
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
