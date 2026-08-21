---
name: browserbase
description: Drive a real hosted browser and hand it to the user when a page needs a human — a login form, a captcha, 2FA, or a consent wall. Use it whenever a task needs a website that has no API, or when a sign-in blocks you — you get a live-view link the user opens to take over, then you carry on in the same browser session once they reply. Requires the Browserbase connector.
---

# Hosted browser sessions

Some sites have no API and cannot be automated end to end: they want a
password, a one-time code, or a captcha. This skill is how you get through
that — you drive a hosted browser, and when it needs a person you hand them a
link to the very same browser, wait for them to reply, and continue where they
left off.

Two separate things have to be present, and they come from different places:

- **The Browserbase connector** supplies the tools that drive the browser.
- **`browserbase_live_view`** turns a session id into a link a human can open.

## Step 1 — check what you actually have

Look at **your own tool list** before planning anything. The connector's tools
are named after whatever the user called the connection — usually
`mcp_browserbase_start`, `mcp_browserbase_navigate`, `mcp_browserbase_act`,
but `mcp_bb_start` if that is what they typed. Match on the `_start` /
`_navigate` / `_act` / `_observe` / `_extract` endings, never on a name you
assumed.

**Do not trust the `## MCP Tools (live registry)` section of your system
prompt here.** It is built from the server's shared connector catalog and does
not know about per-user hosted connections, so it can say nothing is connected
while these tools sit in your tool list. Your tool list wins. If your roster is
large enough that MCP tools are behind `tool_search` / `tool_call`, search for
"browserbase" before concluding anything is missing.

Then handle what is actually absent:

- **No connector tools.** Say so in one turn and stop. The user needs to add
  Browserbase under **Settings → Connections** *and* switch it on in **this
  chat's** Tools picker — a chat started before the connection was added
  will not have it. Do not try an `mcp_*` call to find out; it will not resolve.
- **Connector present, no `browserbase_live_view`.** You can still drive the
  browser, but you cannot mint a link. For a login or captcha, tell the user
  to open <https://www.browserbase.com/sessions>, click the running session,
  and use its live view — that authenticates with their own Browserbase login
  and needs nothing from this server.
- **Minting says no credential is available.** The tool uses the user's own
  Browserbase connection key, so this means the connection is missing, was
  added without a key, or is switched off in this chat. Point them at Settings
  → Connections and the Tools picker; do not ask them for a key in chat.
- **It asks for an explicit `session_id`.** This server is using a shared
  key, so it will not pick a session for you — the running one might be
  another user's. Use the id `_start` returned, or suggest the user add their
  own Browserbase connection.

## Step 2 — start the session

Call the connector's `_start` tool, then `_navigate` to where you need to be.

If you can read a session id in the result, **put it in your reply text** so
it survives into the next turn's history. If you cannot — the connector may
return a screenshot, and a tool result carrying binary is suppressed before you
see it — that is expected, not a failure. Do not call `_start` again to chase a
readable id: every extra call abandons the browser you were working in.

## Step 3 — mint the live view

Call `browserbase_live_view`. Pass `session_id` when you have it; **omit it**
and the tool uses your one running session, which is the way through when the
id was suppressed. If several sessions are running it refuses rather than
guess — end the stale ones, or pass the id of the one you started.

If it reports `BROWSERBASE_LIVE_VIEW_NOT_READY`, call `_navigate` once (any URL)
and mint again — Browserbase only publishes a view once a browser has attached.

## Step 4 — hand it over, then stop

When you hit the login, the captcha, the 2FA prompt — do not try to solve it,
and do not keep the turn alive waiting. **You cannot wait for a human here.**
There is no pause: `ask` and `sleep` do not exist in interactive chat, and an
approval card runs a single tool without giving you another turn. So in one
message:

1. Post the link as a plain markdown link — it opens in a new tab.
2. Say exactly what you need them to do there.
3. Warn them that **anyone who opens that link can drive that browser**, so
   they should not forward it or share this chat while the session is live.
4. Ask them to reply when they are done.
5. **End your turn.**

In a scheduled run there is no one watching the transcript, so deliver the
link with `ask` (which parks the run until they answer) or `notify` instead.

## Step 5 — resume in the same browser

When the user replies, pass **the same session id explicitly on every
connector call** — `_navigate`, `_act`, `_observe`, `_extract` all accept it.
Do not rely on the connector remembering which session is current: this server
may open a fresh transport between turns, and an explicit id is what reliably
reattaches. If you never got a readable id, mint the link again with no
`session_id` to have the tool tell you which session is running, and use that.

Do **not** call `_start` again — that is a new, blank browser and their login
is gone.

## Step 6 — finish, then tear down

Call the connector's `_end` tool when the work is done **and** the user has
confirmed they are finished with the link. Ending the session kills their
view, so never do it in the same turn you hand the link over, and never as a
reflexive tidy-up while they are still working.

## The link is a capability, not a bookmark

The live-view URL needs no password. Whoever holds it controls that browser —
including anything the user has just logged into — until the session ends.
Treat it accordingly: give it to the user, say plainly what it grants, and
never paste it anywhere it will outlive the session. Fleet cannot revoke it;
only ending the session or its expiry stops it working.

## When something has gone wrong

- **"The link showed a disconnect message."** The session ended. Start a fresh
  one, mint a new link, and tell them that is what happened rather than
  implying they did something wrong. Sessions normally survive between turns;
  if they repeatedly do not, the account may lack keep-alive (a paid feature),
  which is worth passing on to the user.
- **`BROWSERBASE_SESSION_NOT_VISIBLE`.** The session ended, or this server's
  key and the connector's key belong to different Browserbase projects. Pass
  that distinction on — the user can check it, you cannot.
- **A step keeps failing on the page itself.** Prefer `_observe` to see what
  is actually there over guessing at `_act` phrasings, and say what you tried.

## What this is not

Fleet has no in-sandbox browser. Browser automation is a connector, which is
why everything above depends on one being connected — there is no local
fallback to reach for, and you should not imply there is.