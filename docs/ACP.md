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
            mcpServers = {}, -- fleet ignores client-supplied MCP servers (its first reply would say so), so send none
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
| `initialize` | Protocol 1; `agentInfo` `fleet` + the build version; capabilities: text prompts, embedded text resources (`promptCapabilities.embeddedContext`), and `loadSession: false`. No auth methods, no image or audio, no `mcpCapabilities` (client MCP servers are ignored; see `session/new`). |
| `session/new` | Checks the configured user and token with the server, then records a session. The check is one `GET /me` with the same headers a prompt sends, bounded at 10 s; `/me` sits behind the same IP filter, shared-token check and membership check as `POST /chat`. A refusal fails `session/new` with `auth_required` and the text the first prompt would have got (see Errors), so it shows when the session opens instead of after a prompt has been typed: a wrong token, an email that is not a fleet user, a viewer-role user, an address the IP filter refuses, or a 401. For fleet's own refusals the text is identical; a proxy's 401 or 403 can depend on the path. `/me` refuses no read, so a viewer is recognised by the `role` it reports and gets the same text a viewer's prompt does. An answer that says nothing about the identity opens the session anyway, and the first prompt reports any real problem: no answer at all (a server that is down, unresolvable, or silent for the 10 s; left to the prompt on purpose, because some clients, such as CodeCompanion.nvim, show a `session/new` error only as a notice without its text, but a prompt's error in the chat), a 404 (a server older than `/me`: a newer `fleet` binary may run `fleet acp` against an older server), a 5xx, any other 4xx, or a 200 whose body is not the expected JSON. The fleet conversation is created by the first prompt, like a new web chat. A missing email or token is found before the server is asked. Client-supplied `mcpServers` are accepted and **ignored**: `fleet acp` never starts, contacts, stores or forwards them, because fleet's tools and connectors come from the operator's bundle and run host-side with brokered credentials. They are not refused, because clients send every MCP server their user configured to every ACP agent: Zed forwards the servers in its `context_servers` setting (Zed 1.22.0 showed "Failed to Launch — Invalid params" while `fleet acp` refused them) and Emacs agent-shell its `agent-shell-mcp-servers`, so a refusal left such a user unable to open a session at all. Only the servers' names are used and kept. An entry can carry secrets in its `env`, `headers`, `args`, `url` or `_meta`, and `fleet acp` never logs, shows or keeps any of those. `fleet acp` writes one line to stderr at `session/new`, each name quoted (`fleet acp: ignoring 1 MCP server sent by the client ("dummy-mcp"); fleet's connectors come from the operator's bundle`), and the user is told once per session: the first prompt that gets the session and is not cancelled opens with an `agent_message_chunk` of its own paragraph, ``fleet does not use the MCP server your editor sent (`dummy-mcp`): fleet's tools and connectors come from the fleet operator and run on the fleet server.``, sent just before its submission. So it comes ahead of anything the turn streams and goes out whatever the prompt's outcome: answered, queued by fleet, or failed. Later prompts in the session, a resend of that first prompt included, do not repeat it. A prompt that ends before that point (refused content, over the waiting cap, cancelled before it got the session, the client gone) leaves the notice for the next one. A `session/cancel` (or the client going away) that lands while the notice is being written is honoured before the submission: the prompt answers `cancelled` and is never sent to fleet, and the notice, which has gone out, is not repeated. The notice is not part of the fleet conversation, and `text.replace` is reconciled against the turn's own text without it, so it never causes a `— revised answer —`. Each name is shown on one line: control and space characters (line breaks, tabs, escapes) become a space, Unicode format characters (category Cf: bidi overrides, zero-width spaces) are dropped, runs of spaces collapse, and the name is cut at 60 characters. Other invisible characters (U+3164, variation selectors) pass through, harmless inside the Markdown inline code the notice puts each name in. At most 5 names are listed, then "and N more"; an entry with no name shows as "unnamed" (`""` on stderr). `acp-go-sdk` decodes each entry before `fleet acp` sees it: a `null` entry, `{}` and an entry of a transport the SDK does not know decode (as HTTP) and are ignored like the rest, but an entry the SDK cannot decode makes the SDK itself answer `session/new` with invalid params (see Deviations and limits). |
| `session/prompt` | One `POST /chat` turn. Later prompts in the session continue the same fleet conversation. The conversation id is taken from the `X-Fleet-Conversation-Id` response header as well as the first frame, so a stream that dies early does not start a second conversation on retry. Each prompt carries an idempotency `input_id`: the client's `messageId` when it sends one (echoed back as `userMessageId`, and scoped to the ACP session, since clients may number messages per session), otherwise a key that a retry of the same text reuses after a lost answer. fleet records that key whether the prompt started a turn directly or was queued (`docs/INPUT-QUEUE.md`), so a resend within the input queue's retention window (`docs/INPUT-QUEUE.md`) is answered with the original input and never run twice. The reply then says the message is already running or has finished, was already taken (how that turn ended, its reply or an error, is in the conversation), or is still queued (with its place in line when fleet reports one), and where to follow it. A prompt sent while an earlier prompt on the session is still open never stops it: prompts on a session run one at a time, so the new one waits for the session while the earlier one runs on and answers with its own outcome. Waiting prompts take the session in the order `fleet acp` received them (the order its handlers registered them: `acp-go-sdk` runs each request on its own goroutine, so two prompts sent at almost the same moment may be received in either order). If the new one carries the same key (the same `messageId`, or the same text while an earlier attempt's outcome is unknown), it is then answered with that input as above and never run twice; otherwise it runs as its own turn. At most 20 prompts wait for a session behind the one running (the same bound as fleet's own input queue, which cannot see prompts that have not reached it yet); one more is refused at once with an internal error saying the session already has a prompt running and 20 waiting, and is never sent to fleet, so the client can send it again once they are answered. Each prompt with an unknown outcome (a lost answer, or a 5xx, which can follow an input fleet committed), or whose cancel fleet did not confirm, keeps its own key until it is answered, so unrelated prompts in between do not break a later retry; these keys are never evicted (a forgotten key would let its retry run twice), so they cost one small entry per lost answer for the life of the session. A prompt whose answer was lost while it was being cancelled is stopped by its key (`POST /conversations/{id}/cancel` with `input_id`): fleet withdraws it if still queued, cancels its turn if running, and refuses to launch it if its turn has not registered yet, atomically with registration; if the prompt has not reached fleet at all, fleet takes its key with a cancelled record, so the late arrival never runs. A Stop fleet does not accept, or sends but cannot confirm in time (and the turn's stream does not confirm either), is reported as unconfirmed, and one that reached fleet after the turn had already finished (whether the Stop named the turn or the input's key, the turn's final frame arrived first, or fleet accepted the Stop but the turn completed, or failed on its own, in the same instant, which the adapter learns by reading on to the turn's final frame) says so: the prompt still ends `cancelled`, as ACP requires, with a note that nothing was stopped and what the turn did stands. A retry of an unresolved prompt goes to the conversation it was first sent to (none, for a session's first prompt), since fleet recognises a key only there; it does not move the session to another conversation. If fleet reports that an earlier attempt was accepted and cancelled, the prompt is not resubmitted: fleet does not record why an input was cancelled, and a Stop from any surface looks the same as a launch that failed, so resubmitting could run a message after its Stop succeeded. The reply says it did not run and asks for it to be sent again as a new message (a new `messageId`). A text-only prompt whose lost answer's Stop fleet confirmed drops its key, so sending the same text again runs it rather than replaying the cancelled attempt. Every same-text prompt that was already waiting behind that Stop is a resend of the one message: the first to get the session runs it under a fresh key and the rest reuse that key, so it runs once, and the rest are answered with that run. A prompt cancelled while fleet was queueing it, or whose resend was answered with an earlier attempt that is still queued or running, is stopped by its key the same way. If the conversation already has a running turn (started from another surface, such as the web chat), fleet queues the prompt to run after it; the prompt then ends `end_turn` with a note saying it was queued, its place in line when fleet reports one (next, or how many other queued messages run first), and where to follow it, rather than an error inviting a retry. The response carries `_meta["fleet.conversationId"]` and token `usage`. |
| `session/cancel` | Stops the turn **server-side** (`POST /conversations/{id}/cancel`, scope `turn`) and answers the prompt with stop reason `cancelled`. Closing the HTTP stream alone would not stop the turn, because fleet detaches a turn from its request by design. If fleet does not accept the Stop, the prompt still answers `cancelled` (ACP requires it), but the transcript says the turn may still be running and where to stop it. A turn stopped from another fleet surface, such as the web chat's Stop, also ends as `cancelled`, including a Stop that cancelled the prompt before its turn started. The Stop names the watched turn (`turn_id` from `turn.started`), and the server cancels it only while it is the running turn, so it can never hit a follow-up queued from another surface. The turn id comes from the `X-Fleet-Turn-Id` response header or the `turn.started` frame. A turn already reported as over is never sent a Stop, and an untargeted Stop is never sent. Without the turn id, the prompt is stopped by its key instead (`input_id`, as under `session/prompt`): fleet withdraws it if still queued, cancels its turn if running, and the prompt reports a confirmed stop, an input that had already finished, or an unconfirmed stop, whichever fleet answers. Only when no conversation is known either is the stop reported as unconfirmed. It reaches every prompt open on the session when it is handled: the one running a turn, and any still waiting for an earlier prompt on the session, which are never sent to fleet. The SDK does not keep the order of a prompt and a cancel sent at almost the same moment, so rarely such a prompt is cancelled too, or escapes the cancel and runs after the stopped turn. A later `session/prompt` is not a cancel, although `acp-go-sdk` cancels the earlier request's context when one arrives on the session: `fleet acp` keeps its own stop for each prompt, so the earlier prompt runs on (see `session/prompt`). A `$/cancel_request` naming a prompt arrives as that same context, cannot be told apart from it, and is ignored, which ACP allows for `$/` notifications. |
| `session/close` | Forgets the session. The conversation stays in fleet like any chat. |
| Client gone: stdin closed (the client exited, crashed or ended the session), a write to stdout failed (the client stopped reading), or `SIGTERM` / `SIGINT` / `SIGHUP` to `fleet acp` | Handled as a `session/cancel` of every prompt still open: a running turn is stopped **server-side**, as above, and a prompt still waiting for its session is never sent to fleet, nor is one that arrives afterwards: a waiting prompt leaves at once, without waiting for the turn ahead of it to finish stopping. A prompt fleet queued behind another turn was already answered (`end_turn`, with the queued note), so it is not open, and it still runs after the client has gone. With nothing in flight, nothing is sent and `fleet acp` exits at once. Otherwise it waits for those Stops before exiting, for up to 20 seconds: a turn submitted just before may need up to 6 s to name itself, the Stop request has its own 10 s limit, and fleet's confirmation up to 3 s more. Against a healthy server the wait is one round trip. If the bound runs out, `fleet acp` exits anyway and says on stderr that a turn may still be running, naming the conversations to check (a rare slow path gets there after its Stop was answered, hence "may"). Whether any of this happens depends on how the client ends `fleet acp`. One that closes its stdin, or signals it and lets it exit, gets the Stop (checked live with Neovim's CodeCompanion.nvim). One that kills it outright, or before a slow fleet has taken the Stop, does not (see Deviations and limits). Once the client is gone nothing more is written to stdout; stderr says how each stopped turn ended and, when fleet did not confirm a Stop, where to stop it by hand. stderr belongs to the client too, though, and once the editor has exited it is often gone as well (the write then fails, harmlessly), so after an editor exits mid-turn, check the turn in the web chat. A stderr nobody reads holds the exit up by at most a second past the bound. A write to a stdout the client has closed fails without killing the process (`SIGPIPE` is ignored), so the Stop still goes out. A second signal ends the process at once. A `SIGHUP` or `SIGINT` that `fleet acp` was started with ignored (`nohup`, a background job) stays ignored; Go keeps no inherited ignore of `SIGTERM`, so a `SIGTERM` is always handled. `SIGKILL` cannot be handled (see Deviations and limits). |
| `authenticate`, `logout`, `session/load`, `session/list`, `session/resume`, `session/set_mode`, `session/set_config_option` | Not advertised. They answer method-not-found. |

Stream events map onto `session/update`:

| fleet SSE (`POST /chat`) | ACP `session/update` |
| --- | --- |
| `text.delta` | `agent_message_chunk` |
| `reasoning.delta` | `agent_thought_chunk` |
| `tool.call` / `tool.result` | `tool_call` (title = tool name, `in_progress`) / `tool_call_update` (`completed` or `failed`; `pending` for a call that fleet staged as an approval card, whose placeholder result is not a failure; a call whose staging itself failed stays `failed`) |
| `text.replace` | fleet's final text is the turn's last model step that wrote text, not the whole turn, so it is compared with the text streamed since the last tool call (or with the latest step that streamed text, and failing that with the whole turn), ignoring surrounding whitespace. Nothing when it matches; the missing suffix when it extends it; otherwise the final text after a `— revised answer —` line (see below) |
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

The JSON-RPC `message` of each error below, except the last two rows, is its
reason (the text described there), on one line. It is not the generic name of
the error's code ("Authentication required", "Internal error", "Invalid
params"), which acp-go-sdk would put there and which some clients show alone:
Emacs's agent-shell shows the code and `message` and keeps the rest behind its
Details button. Nor is that name put in front of the reason: the client shows
the code beside it, and the name would mislabel most reasons (a viewer's
refusal is not a failed authentication, nor a timeout an internal error). Line
breaks in a quoted server reply become spaces. A reason longer than 400
characters (in practice one quoting a reply, such as a turn's error or a
proxy's page) keeps as many whole sentences as fit, when they fill at least
half of that; otherwise it is cut after the last whole word that fits (a
"word" over 40 characters, such as a long URL, is cut where the limit falls)
and ends in "…". A reason that quotes a reply says what to do before the
quote, so the cut keeps it. The `code` is unchanged, so a client that acts on
it, as JSON-RPC intends, works as before. `message` is only ever the reason or
a cut of it. `data` is left out when `message` holds the whole reason (only
its whitespace changed), and is `{"error": …}` with the reason in full only
when `message` was cut: Zed shows an error as its `message` followed by its
`data` as JSON, so a reason whole in both showed twice, while
CodeCompanion.nvim reads `data.error` when it is there and `message`
otherwise, and agent-shell shows `message`.

| Failure | What the ACP client sees |
| --- | --- |
| No email / no token configured | `initialize` still succeeds; `session/new` answers `auth_required` (-32000) with the same fix-it text `fleet chat` prints. `fleet acp` keeps running, so the client shows the reason instead of "agent exited". |
| Refused by the server (403) | `session/new` answers `auth_required`, with text naming the reason the server gave: a wrong shared token names `FLEET_SERVER_TOKEN`; an email that is not a provisioned fleet user (`not_a_member`) names the user and `fleet chat user add <email> --password -`; a viewer-role user (`read_only` on a prompt; the `viewer` role `GET /me` reports at `session/new`) names the user and `fleet chat user role <email> --role member`; a client address the server's IP filter refuses names `FLEET_IP_ALLOWLIST` / `FLEET_IP_DENYLIST`, without saying which list matched (the server does not say either). Any other 403 body (a reverse proxy's page, say) is quoted as a short one-line excerpt instead of being blamed on the token. The token value is never included. A refusal that starts after the session opened (a role changed mid-session, say) is answered the same way on the next prompt. |
| Any 401 | `session/new` answers `auth_required`, with text naming the user (or the next prompt does, when the 401 starts after the session opened). fleet itself sends a 401 here only for a revoked web session, which `fleet acp` never presents, so in practice it comes from a proxy in front of fleet. |
| Any other error status from the server | Internal error (-32603) quoting the status and the server's reply |
| Daemon down | Internal error (-32603) naming the unreachable server URL, on the prompt: `session/new` opens the session when the server cannot be reached (see `session/new`). |
| Identity check inconclusive at `session/new`: no answer, a 404 (a server older than `/me`), a 5xx, any other 4xx, or a 200 that is not the expected JSON | The session opens, and the first prompt reports any real problem as above. |
| `turn.error` / `turn.model_required` | Internal error carrying the server's message |
| `--timeout` exceeded | The turn is stopped server-side, then an internal error names the timeout and the flag. If the Stop fails, the error says the turn may still be running and where to stop it, then why the Stop failed. |
| A prompt beyond the 20 waiting for a session | Internal error saying the session already has a prompt running and 20 waiting (see `session/prompt`) |
| An image, audio or binary blob in a prompt | Invalid params (-32602) naming what is refused |
| Unknown session id | -32002 resource not found, naming the session and saying it was closed or opened by an earlier `fleet acp` process. Like every error here, it has no `data` unless its message had to be cut: the client sent the id, and `message` names it. |
| A request acp-go-sdk cannot decode or validate | The SDK's own `Invalid params`, with the generic message and the decoder's text in `data.error`: the SDK refuses the request before `fleet acp` sees it |
| An unsupported method | -32601 with the generic message `Method not found` and the method in `data.method`, whether `fleet acp` itself or the SDK answers it |

## Honest scope

What shipped:

- `fleet acp`, an in-tree stdio ACP agent in the one `fleet` binary, covering
  `initialize`, `session/new`, streaming `session/prompt`, `session/cancel` and
  `session/close`.
- Turns go through the running server's governed chat seam and are visible in
  the web UI as ordinary conversations owned by the configured user.
- Tests in `internal/acp` drive a real SDK client over pipes against a fake
  server that speaks the `POST /chat` SSE contract. They cover streaming, the
  follow-up turn, `text.replace` reconciliation, cancel (including a prompt
  still waiting for the session), a prompt resent or followed by another
  while a turn runs (one run, no Stop), `$/cancel_request`, timeout, 403,
  daemon-down, `turn.error`, policy refusal, approval pointers, refused
  content, the reason in each error's `message` (with its `code` unchanged,
  and `data` only when the message is cut, checked on the raw JSON-RPC as
  well), the stdio entry point (stdout carries only JSON-RPC), and the
  no-execution-imports guard. No live model, no live Buzz in CI.
- The identity check at `session/new` is covered by tests on both sides.
  `internal/chattui` runs `CheckIdentity` against a test server answering
  `GET /me` with a member, an admin, a viewer, each 403 body fleet writes
  (wrong token, `not_a_member`, the IP filter), a proxy's 403 page and a 401
  that echo the token (redacted), a 404, 500, 502, 429 and 400, a malformed
  200 and a 200 that is not JSON, plus a refused connection and a server that
  never answers; it pins each exact text, that the viewer text is the one a
  viewer's prompt gets, and that the token appears in none. `internal/acp`
  drives `session/new` through a real SDK client: `auth_required` with the
  exact reason for a viewer, a non-member, a wrong token, the IP filter and a
  401; a session that opens when `GET /me` gets no answer, and runs a prompt
  once the server answers; a session that opens, and runs a prompt, for a
  member, an admin, a 404 and a 500; and a refusal that starts after the session opened
  still reaching the prompt.
- Checked live against a real `fleet serve` on 2026-10-07. In Zed 1.22.0, a
  viewer, an unknown user and a wrong token failed `session/new` with
  `auth_required` (Zed shows the reason in its "Authenticate to fleet" view
  when the error's `message` carries it), a member's session opened and
  answered, and with the server unreachable the session opened and the
  prompt reported it. In Neovim's CodeCompanion.nvim, a viewer's refusal at
  `session/new` shows as an error notice holding the raw error, reason
  included, rather than in the chat as a prompt's error does, and an
  unreachable server is still reported in the chat. Emacs agent-shell and
  Buzz were not re-checked.
- Seen live with Zed 1.22.0 on 2026-10-07, before the identity check: Zed
  answers an `auth_required` error on a prompt with its own fixed
  "Authentication Required" text, whatever the error says, so a reason the
  server gave only at the first prompt never reached the user. That is why
  the check runs at `session/new`.
- Tests in `internal/acp` also cover the client going away mid-turn through
  the real entry point. In-process on pipes: stdin closed, a signal, prompts
  waiting in the session's line (they leave at once, unsent), a Stop that
  never answers (the bound), a stderr nobody drains, a prompt sent after the
  signal, and nothing in flight. As a real child process with real
  signals: `SIGTERM`, `SIGINT` and `SIGHUP` each stop the turn before the
  exit, a second `SIGTERM` ends the process at once, a broken stdout stops the
  turn with stdin still open, and a `SIGHUP` it was started with ignored stays
  ignored.
- Tests in `internal/acp` also cover client-supplied MCP servers. `session/new`
  with stdio, HTTP, SSE and ACP-transport servers succeeds, and none is
  started, contacted or sent to fleet (a stdio command that would leave a
  marker file, HTTP and SSE URLs on a listener that counts requests). The
  first prompt's updates open with the notice; a later prompt, and a session
  that sent no servers, get none. Through the real entry point, with Zed's
  entry verbatim, an unknown transport and an empty entry, distinctive
  secrets in an env value, a header value, an arg, URL queries, `_meta`, the
  command and the env and header names appear nowhere on stdout, on stderr or
  in what reached fleet. Names with line breaks, escape sequences and bidi
  overrides are shown on one line, long names and long lists are cut, a
  Markdown parser reads each name as inline code, and stderr quotes each
  name. `text.replace` still reconciles with the notice in front. A first
  prompt queued by fleet, one with a prompt waiting behind it, one that
  fails, a lost answer and its resend, prompts that end before they are
  submitted, and a `session/cancel` that lands while the notice is being
  written each behave as described under `session/new`. A `null` entry is
  accepted and shown unnamed. Entries the SDK cannot decode (`env` or
  `headers` as an object, `args` as a string, an entry that is not an
  object) are refused by the SDK with an error that carries none of their
  values.
- Checked against a real `fleet serve` on 2026-10-07, with client MCP
  servers. Zed 1.22.0 with an MCP server in its `context_servers` (a build
  that refused them showed "Failed to Launch — Invalid params" there): the
  thread opened, the first reply started with the notice, the second had
  none, and Zed's log showed the stderr line. Emacs agent-shell 0.85.3 with a
  global `agent-shell-mcp-servers` and no per-agent `:mcp-servers` override:
  the session opened and the first reply started with the notice. Zed was
  rechecked with the final wording and the quoted stderr names; a cancel that
  lands while the notice is being written is covered by a test, not live.
- Checked against a real `fleet serve` on 2026-10-05: a retry with the same
  `messageId` while its turn ran (one run; the original ended `end_turn` with
  its answer, and the retry got the replay note), a dropped connection
  followed by a text resend (the replay), a different prompt mid-turn (nothing
  stopped; it ran after), and, unchanged, an explicit Stop mid-turn, a late
  Stop ("already finished"), an unconfirmed Stop with the server paused, and
  `--timeout`.
- Checked against a real `fleet serve` on 2026-10-05, with the client going
  away mid-turn: `SIGTERM`, stdin closed, a simulated crash (both pipes
  closed) and a real Neovim `:qa!` each made `fleet acp` exit in about 0.1 s,
  and the server recorded `turn.cancelled`. `kill -9` left the turn to run to
  completion, as documented. A build without this handling, sent `SIGTERM`,
  let the turn complete. Rechecked on 2026-10-06 with the per-session line
  merged in: `SIGTERM`, stdin closed, a simulated crash, a broken stdout with
  stdin still open and Neovim `:qa!` each exited in about 0.1 s with
  `turn.cancelled`; `kill -9` again left the turn to complete; and a second
  prompt waiting in the session's line at the `SIGTERM` was never sent to
  fleet.
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
- **Streamed text is append-only.** ACP cannot retract a chunk. When fleet's
  final answer replaces a draft that was already streamed (for example, it
  stripped a tool call the model wrote into its final answer, or retried a
  model call that had already streamed part of a reply), the client gets the
  final answer again after a `— revised answer —` line, so it ends on what
  fleet persisted. Text a model writes before a tool call also stays in the
  client's transcript, as does a tool call it wrote as text that fleet then
  ran for real; the web chat drops both when the turn ends, because fleet's
  final answer is only the last step that wrote text.
- **Tool detail stays in the run log.** Tool calls appear as titled
  `tool_call` updates with a status. Inputs and outputs are not forwarded.
- **The client's filesystem and terminal are not used.** Tool calls run in
  fleet's sandbox workspace. The session `cwd` is recorded, not mounted. A
  client that shares a file by sending only its path, rather than an embedded
  resource, loses that context: fleet cannot read the path. CodeCompanion.nvim
  does this by default; the configuration under "Neovim (CodeCompanion.nvim)"
  works around it.
- **The client's MCP servers are not used.** ACP's
  [session setup](https://agentclientprotocol.com/protocol/session-setup)
  says "All Agents **MUST** support connecting to MCP servers via stdio" and
  "Agents **SHOULD** connect to all MCP servers specified by the Client".
  fleet takes the list and ignores it: fleet's tools and connectors are the
  operator's, run host-side, and a client's servers run on the client's
  machine. The user is told once per session, in the first reply (see
  `session/new`).
- **An MCP server entry the SDK cannot decode still fails `session/new`.**
  `acp-go-sdk` decodes each `mcpServers` entry before `fleet acp` sees the
  request, and answers invalid params itself (`invalid variant payload` or
  `no matching variant for union`, with none of the entry's values) for an
  entry that is not a JSON object, or whose transport it recognises (by
  `type`, or a stdio entry by its `name`, `command`, `args` and `env` keys)
  but which has a field of the wrong JSON type: `env` or `headers` as an
  object (the `mcp.json` shape) rather than ACP's list of `name` and `value`
  pairs, or `args` as a string. Such a client cannot open a session until the
  entry is fixed or left out: the refusal comes before `fleet acp` could
  ignore it. An entry with no `type` that does not match its transport is
  tried as each transport in turn: one it fits is ignored like the rest, and
  one it fits none of is refused the same way.
- **No `session/load`.** A session lives as long as the `fleet acp` process.
  The conversation itself persists in fleet, but resuming it over ACP is
  deferred until the session id can round-trip honestly.
- **Stop reasons are `end_turn`, `cancelled` or `refusal`.** A fleet ceiling
  (cost, tokens, iterations) ends the turn through fleet's normal path, and the
  adapter does not guess `max_tokens` / `max_turn_requests` from it.
- **One identity per process.** Every turn runs as the configured fleet user.
  Per-Buzz-user mapping is out of scope.
- **A force-killed `fleet acp` cannot stop its turn.** `SIGKILL` (`kill -9`,
  or a client that kills its agent outright) ends the process before it can
  send a Stop, and so does a client that kills it before a slow fleet has
  taken one. The turn keeps running server-side until it ends or reaches one
  of fleet's own ceilings; it stays visible in the web chat, and can be
  stopped there. Zed's source (read on 2026-10-06, not tested live) kills its
  agent's process group with `SIGKILL` when it drops the agent connection,
  without a `SIGTERM` or closing stdin first, so when Zed ends `fleet acp` that
  way the turn keeps running. Whether a plain Zed quit takes that path was not
  checked.

Deferred: a Buzz Desktop catalog entry (a custom command works today),
`session/load`, and an ACP *client* in fleet (launching other ACP agents is a
different feature).
