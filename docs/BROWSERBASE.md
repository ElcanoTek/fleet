# Browserbase: hosted browser sessions with a human handoff (#987)

Fleet has no in-sandbox browser. [ADR-0044](adr/0044-remove-in-sandbox-browser-tool.md)
deleted the native `browser` tool on 2026-08-15 — it could not run on the shipped
sandbox image, its real Chromium path was never exercised in CI, and it was DOM-only
with no credential or captcha story — and named Browserbase as the replacement path.
Its consequences section states the position plainly: *"fleet again has no first-party
way to drive an API-less site without a connector."*

The Browserbase **connector** (a remote MCP server in the built-in directory) has been
usable for a while: the agent can drive a hosted browser with `start` / `navigate` /
`act` / `observe` / `extract`. What was missing is the part #987 was actually about —
**getting a human into that browser** when a page wants a password, a one-time code, or
a captcha.

## What shipped

| Piece | Where |
| --- | --- |
| `browserbase` built-in skill | `internal/clientconfig/builtin_skills/browserbase/SKILL.md` |
| `browserbase_live_view` native tool | `internal/tools/browserbase_live_view.go` |
| `keepAlive=true` on the connector URL | `internal/clientconfig/builtin_remote_catalog.yaml` |

The flow: the agent calls the connector's `_start`, gets a session id, mints a
live-view URL with `browserbase_live_view`, posts it to the user, and **ends the
turn**. The user opens the link, completes the login, and replies. The agent resumes by
passing that same session id back to the connector's tools — the browser is where they
left it.

## Why the live-view URL needs a native tool

The hosted MCP exposes six tools, and `start` returns only a session id. No MCP tool
returns a viewer URL. That comes from `GET https://api.browserbase.com/v1/sessions/{id}/debug`
with an `X-BB-API-Key` header, which needs a credential — and by invariant credentials
never enter the sandbox or the model context. So minting the URL has to be host-side Go.

`browserbase_live_view` is a new member of the **"host network / brokered fetch"**
exception class already enumerated in
[ADR-0036](adr/0036-sandboxed-file-tools-and-host-io-exceptions.md), alongside
`web_fetch`, `download_url`, `generate_image` and the search tools: one authenticated
GET to a fixed public vendor host, no model-authored code, no sandbox bypass. It drives
no browser and renders no page — driving stays with the connector, exactly as ADR-0044
decided. No invariant is weakened; one host-side exception joins a list that exists to
be enumerated rather than silent.

## One key, resolved connector-first

The credential is resolved in this order:

1. **The running user's own Browserbase connector key**, unsealed host-side from
   `remote_mcp_servers.api_key_enc` via the resolver `internal/agent` already declares
   (`ConnectedServersForUser` + `AcquireTokenByID`, which for an `api_key` connection
   returns the unsealed key and registers it for literal redaction).
2. **`BROWSERBASE_API_KEY` from the host env**, when there is no per-user connection.

So in normal use you paste the key **once**, in Settings → Connections, and both driving
the browser and minting the link use it. That also makes the capability per-user rather
than box-wide: a user with no Browserbase connection cannot mint against someone else's
session id.

The connector is matched by its **URL host**, not its registration name, because the name
is whatever the user typed when they added it.

The env fallback exists for paths with no per-user connection — notably scheduled runs,
which do not wire the task owner's connection through today (that is a follow-on, and the
call site says so rather than leaving a silent gap). It is also there for an operator who
prefers one box-wide key. Two things to know if you use it:

- It is **workspace-wide**, so any user of this fleet can mint a live view for any
  session id they can obtain — and session ids reach model context, therefore
  transcripts.
- If it belongs to a **different Browserbase project** than the connector's key, minting
  404s for a session that demonstrably exists, which looks identical to "the session
  ended". `BROWSERBASE_SESSION_NOT_VISIBLE` names that case so the user can check it.

Neither applies when the connector key is used, which is why that is preferred.

## The live-view URL is a capability, not a secret

The minted URL needs no login and is iframe-embeddable by design. **Whoever holds it
controls that browser session** — including anything the user has just logged into —
until the session ends. It is not a secret fleet is trying to hide; it is a capability
fleet deliberately hands to a human. Consequences:

- The skill tells the agent to say plainly what the link grants and not to forward it.
- Fleet **cannot revoke it.** Only the connector's `end` tool, or session expiry, stops
  it working.
- Conversations can be shared publicly (`/shared/*`). A minted URL sitting in a shared
  transcript is a published capability for as long as the session lives.
- The skill therefore tells the agent to call `end` when the work is done — but *not*
  in the handoff turn, since that kills the view the user is about to open.

## Session survival across the turn boundary

The handoff requires the turn to end while the user acts, so the session has to outlive
it. **This was verified end to end**: an agent opened a page, handed over the link, ended
its turn; the user navigated somewhere the agent had never seen; the agent then reported
that page and its heading correctly on the next turn.

Browserbase's docs say a session ends when its connection closes unless it was created
with keep-alive, so the connector URL carries `?keepAlive=true` as belt-and-braces. Be
honest about what that observation does and does not establish: the connection used in
testing predates the `keepAlive` flag, so the hosted server appears to hold sessions
across transports on its own, and we have **not** confirmed which mechanism did the work.
Hence:

- `keepAlive=true` is kept because the vendor documents it and it costs nothing, not
  because survival was shown to depend on it.
- Keep-alive is a **paid-plan** feature upstream. If you are on a free plan and a
  handoff loses the session, that is the likely cause.
- An **existing connection keeps its stored URL**, so it will not carry the flag. That is
  only worth acting on if sessions actually fail to survive a handoff — then remove and
  re-add the connection to pick it up. It is not a required migration step.

The other half is reattachment: fleet builds its remote-MCP overlay per turn and may not
present the same `Mcp-Session-Id` next turn, so the skill requires passing the session id
**explicitly on every call**. Browserbase documents this as the supported path for
clients that open a new transport per call, and a stale id surfaces an error rather than
silently redirecting to the wrong browser.

## Configuration

**Normally this is all you need.** Add **Browserbase** under Settings → Connections
with your API key, then enable it in the chat's **Tools picker**. Connector selection is
per-conversation, so a chat started before the connection was added will not have it.
That one key both drives the browser and mints the live-view links.

Getting there is one click: `/settings/connections?connector=browserbase` opens the
connector directory filtered to the Browserbase card with its key form open and focused
— paste, Add, done. The deep link works for any directory entry by name (the built-in
`browserbase` skill hands it to users whose chat has no connector), the entry is on the
directory's Featured shelf, and the add is validated against the vendor before the key
is stored, so a bad paste fails loudly with the form still open.

**Optional box-wide fallback** (for scheduled runs, or an operator who wants minting to
work without each user connecting). Use the guided writer rather than editing the env
file by hand — it prompts hidden, so the key never reaches argv or shell history, and it
upserts into the right 0600 file whether that is `/etc/fleet/fleet.env` or a dev
`.env.local`:

```sh
sudo fleet config set-browserbase-key            # hidden prompt
sudo fleet config set-browserbase-key --key -    # or read from stdin
sudo fleet restart
```

A restart is needed only to make the tool *appear* on a deployment that has no
remote-MCP resolver at all; where connections are available the tool is already
registered, and the key value is read per call, so rotation never needs one.

## Honest scope

- **The tool is absent, not failing, when unusable.** It is registered per turn only
  when a credential is actually reachable: the running user has a Browserbase connection
  **and this conversation has it switched on**, or the operator set a box-wide
  `BROWSERBASE_API_KEY`. That follows the ask/notify rule that the model should never see
  a capability it cannot use, keeps its definition out of the prompt prefix for everyone
  else (the interactive roster already runs near the tool-disclosure threshold), and —
  the part that matters most — keeps the credential and the session enumeration it
  enables inside the same per-conversation gate the connector's own tools obey. Where it
  is absent, the skill falls back to telling the user to open the session from the
  Browserbase dashboard, which uses their own login and needs nothing from fleet.
- **A box-wide key cannot auto-resolve a session.** With no `session_id`, the tool asks
  Browserbase for the running session — but only when the credential came from the user's
  own connector, which scopes it to their project. A shared env key can see every session
  in a project shared by every user of this fleet, so "the one running session" might be
  someone else's logged-in browser, and the URL minted for it grants control. With a
  shared key the tool therefore demands an explicit `session_id`. When several sessions
  are running it refuses either way, and deliberately does **not** list their ids: an
  explicit id is accepted with no ownership check, so printing them would hand over
  exactly what is needed to bypass the refusal, and they would persist in history that
  can be shared publicly.
- **`BROWSERBASE_API_KEY` is now a parent-owned env name.** It is listed in
  `parentOwnedRuntimeEnvNames`, so a client bundle that declares the same key for its own
  MCP connector (for example running the local `@browserbasehq/mcp` server, whose
  documented env is `BROWSERBASE_API_KEY` + `BROWSERBASE_PROJECT_ID`) will now **fail
  server boot** with "connector environment overlaps parent-owned configuration" rather
  than having the value silently scrubbed out of the parent. Failing loud is the
  deliberate choice, but it is a behaviour change for such a bundle: give the connector a
  distinct env name, or drop it from the bundle and use fleet's own tool.
- **The handoff is two turns by construction.** Interactive chat cannot block on a human:
  `ask`/`sleep`/`wake_on_event` are scheduled-only, [ADR-0024](adr/0024-ask-notify-pause.md)
  rejected suspending a live run, and an approval card runs one tool server-side without
  granting a new turn. Continuity comes from the session id, not from holding the turn.
- **No embedded view.** The web proxy sets `frame-src 'self'` and `X-Frame-Options: DENY`,
  and the only iframe the transcript renderer emits is `sandbox=""`. A plain markdown link
  opening in a new tab is the whole affordance, and that is deliberate.
- **The skill is interactive-chat-only.** Bundle skills are not in the scheduled-run
  roster (see [SKILLS.md](SKILLS.md)). The *tool* is available to scheduled runs — and
  scheduled runs are arguably the better fit, since `ask` genuinely parks a run until a
  human answers — so the tool's own description carries the protocol rather than relying
  on the skill being discoverable there.
- **The live-registry prompt section is blind to hosted connectors.** `buildSystemPrompt`
  runs before the per-user overlay opens, and its roster comes from the bundled catalog,
  so the `## MCP Tools (live registry)` section can claim nothing is connected while the
  connector's tools are in the model's tool list. That is pre-existing and affects every
  hosted connector, not just Browserbase; this feature works around it in the skill and
  the tool description, and the underlying bug is tracked separately.
- **Redaction can collide with the URL.** Tool output passes through the shared secret
  redactor, which replaces 8+ characters after markers like `token=` or `api_key=`. A
  viewer URL carrying such a parameter would reach the model as `[REDACTED]`, and
  assistant text is not redacted, so nothing downstream could recover it. The tool
  self-checks and returns `BROWSERBASE_URL_NOT_RELAYABLE` rather than handing back a
  silently-broken link. The fix, if it ever fires, is a redactor-safe rendering — not a
  weaker redactor.
- **Deferred:** no per-page selector. The debug response includes a `pages[]` array, but
  choosing among them means surfacing page titles and URLs — attacker-controlled text —
  into model context precisely when the model is following instructions about handing
  over a capability link. Only the page count is returned. `wsUrl` (the raw CDP channel,
  strictly stronger than the viewer URL) is parsed and dropped.
- **CI cannot exercise the live API call** — it has no Browserbase credential and should
  not gain one. Everything except that is covered by unit tests against an `httptest`
  server, and the end-to-end flow was verified by hand: driving a four-step form/login
  sequence with teardown, minting a link, the two-turn handoff with a page the agent had
  never seen, and the auto-resolved session id. Both live-view URL shapes Browserbase
  returned survived the secret redactor intact, so `BROWSERBASE_URL_NOT_RELAYABLE` is
  dead code on the observed formats — the guard stays because the URL format is the
  vendor's to change, and it already varies between calls.
