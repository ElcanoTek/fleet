# Chat

Chat is the working surface of the platform: the place you ask questions of your
data, analyze it with the assistant, build reports, and review the results. This
guide covers the screen and its controls, how to get answers you can rely on,
what the assistant can reach and what it will ask you before acting, and how the
work you want repeated moves from a conversation into the prompt library and
onto a schedule.

## 1. What Chat is

### Terminal chat

On the server, `fleet chat` opens terminal chat. It signs in with your own
API key, which an admin creates with `fleet sched apikey create <name> --type
user --user you@example.com`; save it to `~/.config/fleet/api-key`. New
threads use the workspace default model by default; `--model` or `/model` selects
another model. `--message "..."` and `--no-tui` support scripts and piped input.
One-shot stdout is buffered until the turn ends so it contains the final reply
instead of superseded drafts; progress and approval notices go to stderr.
One-shot mode prints the conversation ID to stderr; `--conversation <id>` resumes
its model context and loads pending approvals in interactive mode.
Accepting a model suggestion changes only that conversation. `/new` returns to
the workspace default unless you selected an explicit `--model` or `/model`
override; resuming another thread keeps its stored model unless that explicit
override is set.
After accepting a suggestion, messages use that conversation's stored model,
including later changes made by another client; `/model` sets a new explicit
override.
Model suggestions show the target frozen when the card was created. Older cards
without a frozen target must be dismissed before requesting a new suggestion.
A new terminal thread (including `/new`) starts with the same optional
connectors a new web chat would: the ones you have on by default under
Settings → Connections. `--connector NAME` (repeatable) replaces that set for
the new thread, for example `--connector email --connector lens`; an unknown or
unavailable name is refused before anything is sent. `--connector` does not
change an existing thread: use the web Tools picker (or ask the assistant in chat
to enable a bundled optional MCP connector for that conversation).

Routine server updates, restarts, and stops (`fleet update`, `fleet restart`, `fleet stop`) refuse by default while a chat turn, approved action, or scheduled task is in flight, and quiesce new work admission while draining when `--wait` is used (`--wait-timeout` sets the limit, 90 minutes by default; `--force` bypasses the check). If the server restarts during an approved action, its outcome is shown as unknown;
verify the external result before trying another action.

The terminal shows staged actions and a pending count. `/approvals` displays
complete frozen execution arguments, one-line summaries, and deadlines;
`/approve` and `/deny` decide the oldest card,
or take a full approval ID. `/edit {"name":"...","prompt":"...","cron":"..."}`
changes the oldest scheduled-task card locally before approval. Omitted fields
stay unchanged. `cron` can only be changed on a recurring card — not added to a
one-time task, and not cleared (that would make the task run immediately). The
server validates edits when you approve. After an edit, the card's one-line
summary is re-rendered from the edited values, so the name, prompt and cron you
see described are the ones that will be submitted — including the approximate
runs-per-month hint, which is recomputed for the edited cron rather than left
describing the old schedule. A schedule that fires more than about a thousand
times a month is shown as a floor (`≥1000 runs/month`), because the count stops
there. An edited cron the terminal cannot parse shows no hint at all rather than
a guess.
Every staged tool automatically shows its complete frozen execution arguments as
escaped JSON; the one-line summary is not the review record. If the server does
not provide a complete snapshot (or the arguments exceed the 1 MiB review cap),
the terminal refuses to approve. Control characters in reviews, results, and
errors are escaped so they cannot drive the terminal.
Approval-result stdout redirected to a pipe or file preserves the raw result for
automation; direct terminal output remains escaped.

Add `session` to a decision to apply it to future calls of that tool in the same
conversation, or `pattern arg=glob` to restrict that policy to matching arguments.
Patterns use original string argument names, such as `name`, `cron`, or
`to_email` (not the summary label `to`). For executable tools, argument
names match in any letter case, and when a call gives the same argument more
than once, a deny applies if any of its values matches and an approval only if
all of them do. On a scheduled-task or other handler-only card, a pattern
matches a name only as the call writes it, and never one the call gives more
than once, so that pattern leaves the card for you to decide. For the built-in
browser tools, an `action=` pattern also matches the action's usual name, and another name for
the same action in a pattern stands for it (for `playwright_browser`,
`action=start_session` also covers `live`). Spaces are allowed in globs, and a
matching deny takes precedence over approvals. In a deny, `*` and `?` also
match `/`, so `command=rm *` covers `rm -rf /tmp/x`; in an approval they stop at
`/`, so `command=ls *` covers `ls -la` but not `ls /tmp`. `/approvals` shows
the available handler-only pattern arguments; this requires a server that
supplies them.
Executable-tool policies reset on server restart. Scheduled-task and other
handler-only card policies last for this terminal session and still resolve each
card through the server's approval endpoint. `/resume <id>` switches conversation and
loads its cards; `/approvals reload` refreshes cards. Expired cards are removed,
and failed network requests retain the card for retry. One-shot callers use
`--conversation <id> --approve <approval-id>` or `--deny`. A rejected or failed
action returns a nonzero exit code rather than claiming success.

Chat is the conversation with the assistant: you ask, it works, you read the
result and steer. Most of what runs on your deployment took shape here first, in
a conversation someone could watch and correct before it was trusted to run on
its own.

Recurring work usually follows one loop. A conversation produces something worth
keeping. That conversation becomes a saved prompt in the prompt library. The
prompt becomes a scheduled task. The task runs on its own and emails the result
to whoever should see it. This guide covers the first two steps in depth; the
**Background tasks guide** covers the rest.

> conversation → saved prompt → scheduled task → your inbox
> *(this guide covers the first two)*

The two surfaces divide the work simply. Chat is **hands on**: you are in the
conversation, asking, reading, and redirecting as it goes, and that is where a
report gets shaped until it comes back right. Background tasks are **hands
off**: the same conversation, once it is right, runs there on a schedule and
delivers itself, and you only look in when something needs you. The skills are
the same in both places; only your role changes.

Quick-start cards in a new chat can collect a few inputs before starting work.
Text fields keep spaces as you type; larger text boxes accept several lines.
The prompt preview preserves those line breaks so you can check what will be
sent before pressing the card's run button.

### Patrol Launchpad: your first patrol in five steps

The quickest way to put the assistant on patrol is the **Patrol Launchpad**, a
guided wizard that takes you from nothing set up to a running patrol. Open it
with **New patrol** in the **+** menu at the top of the sidebar, on any page
(the **+** stays in the collapsed sidebar; on a phone, open the sidebar
first). The [Patrols page](#the-patrols-page) opens it too: **Set up your
first patrol** when you have no patrols yet, and **Use guided setup** in its
**New patrol** form once you do. Patrols themselves live in Chat — each one is
a chat that wakes on its schedule — and the Patrols page is where you see
them all together.
The Launchpad starts with **Why patrol?**, a four-step Expanding Brain ladder from
chatting, to scheduled reports, to patrols that analyze and decide, to letting
the assistant act on your behalf, plus the trust path:
start at **Suggest & ask** and turn on **⚡ Auto-approve** in the Patrol bar once
you trust its calls. **Show me the plan** then lists the five steps before you
take them:

1. **Hook up your tools.** Every connector you can use is listed; the ones on
   by default for a new chat start ticked, and you tick or untick the rest for
   this patrol. A connector that still needs credentials shows **Set up**, and
   **Connect another tool** opens **Settings → Connections** — both in a new
   tab, and the list refreshes when you come back.
2. **Set your ground rules.** Pick **Who's on watch** (the workspace default
   persona unless you choose another, including your own personas), and save
   your **non-negotiables** — tap a suggestion or write your own and press
   **Save**. Each one is saved straight away as a pinned memory, so the
   assistant reads it in every chat, patrols included; edit or delete it later
   in **Settings → Memories**.
3. **Pick what to watch.** Choose any of **Catch misconfigured deals**,
   **Alert when spend drops off**, **Make sure reports get delivered**,
   **Triage my inbox**, **Suggest optimizations**, **Prep me for my day** and
   **Watch my data for anomalies**, and add anything else in your own words.
   Each card shows which of your ticked tools fit it (**Uses …**) or what kind
   of connector it would need. Then choose how hands-on it is: **Just report**
   (never changes anything), **Suggest & ask** (stages approval cards for you;
   the default), **Act for me** (turns the chat's **⚡ Auto-approve** on and
   lists everything it did), or **Autonomous** (executes live platform actions
   with receipt cards after you accept the Autonomous risk prompt, while
   external messages, calendar invites, schedule changes, destructive storage
   or permission changes, `git push`, and deployment-blocked actions still
   ask). A note under the choice suggests starting at
   **Suggest & ask** and stepping up later.
4. **Choose a rhythm.** **Weekday mornings** (8:00, Monday–Friday; the
   default), **Twice a workday**, **Every 2 hours**, **Every hour**, **Every
   morning** or **Monday kickoff**, or **Custom schedule** for the full
   schedule builder and time zone (your browser's zone by default).
5. **Launch.** Review the plan, the patrol's name and **What it does each
   run** — the instructions the assistant receives every time the patrol
   wakes, pre-written from your answers. **Draft it with AI** asks the
   assistant to write them for you instead; if it cannot, your draft stays.
   Edit anything, then press **Launch patrol**.

Launching creates a new chat with your chosen persona and tools, attaches the
patrol, and runs the first check straight away; **Watch it work** opens that
chat; unless you chose **Act for me** or **Autonomous**, the success screen also suggests
turning on **⚡ Auto-approve** once its calls keep being right. From then on it
is an ordinary in-chat patrol (below): pause it from the Patrol bar, change
or stop it in **Patrols & timers** (the clock on the composer toolbar) or on
the Patrols page, or ask in the chat to change it. Nothing is created until
you press **Launch patrol** — except the non-negotiables you saved, which are
memories as soon as you press **Save**. Close the Launchpad with **Maybe later**
or `Esc` at any point, except while **Draft it with AI** is writing (up to
about 45 seconds) or during the few seconds of launching; your
answers are kept for the rest of the browser session. A read-only account sees
the **Why patrol?** page with a note that it cannot save memories or start a
patrol, and a **Got it** button instead of the rest of the wizard.

### The Patrols page

**Patrols**, by default the first item in the sidebar on every page, opens a
page that lists every recurring patrol you have set up, across all your chats, and under
**Shared with you** the patrols your teammates run in chats they have shared
with you. Search finds a patrol by its name, its chat or its owner. One-shot
timers are not listed here; they stay in their chat's **Patrols & timers**.

Each row shows the patrol's name, its schedule, how many times it has run, its
chat's approval mode and the chat it lives in, with the time until its next
run at the right (or **paused**). On a patrol of your own the row also has
**Run now** and a pause or resume button. Every row ends with **Open chat**,
which goes straight to the patrol's chat without opening the row, on your own
patrols and on a teammate's alike. A patrol whose runs are being skipped
because an approval card is waiting says so on its row, with the number of
runs skipped.

Click a row to open it:

- **Approval mode** changes how much the patrol may do without asking. It is
  the chat's setting, so it applies to every patrol in that chat, and choosing
  **Autonomous** asks you to confirm first.
- **Recent runs** lists the patrol's latest runs: when each fired, whether it
  ran on schedule or because you pressed **Run now**, and when a run was
  skipped, why. It records what happened, not what the assistant found; open
  the chat for that.
- **Each run** shows what the patrol is asked to do every time it wakes;
  **Show full task** expands a long one.
- **Edit schedule & task** changes the schedule, the task and the label, and
  **Remove patrol** stops it for good and takes a second click to confirm.

A teammate's patrol is there to read. Its row carries its owner's initial
and, like every row, **Open chat**; opening the row says whose it is and
shows its recent runs. Only the person who set a patrol up can run, pause, change or remove it,
and what it is asked to do each run is shown only to them.

**New patrol**, at the top of the page, opens a form: say what to check and
do each run, pick a schedule and an approval mode, optionally give it a
label, and press **Start patrol**. Under **Runs in**, **A new chat** makes a
chat for the patrol, named after its label, and **An existing chat** adds the
patrol to one of your chats, beside any patrols it already has (sealed chats
are not offered). **Use guided setup** opens the Launchpad instead. With no
patrols yet, the page offers **Set up your first patrol** (the Launchpad) and
**Or write one yourself** (the form).

### In-chat patrols and timers

You can also run a recurring **patrol** or a one-shot **timer** directly inside
a chat conversation, without leaving the thread or promoting it to a separate
background task. Each time a patrol or timer fires, the assistant wakes up
in the same conversation with its full message history, workspace files, and
enabled connectors intact.

Start a patrol or timer in either of two ways:

- **Ask in chat:** tell the assistant what to patrol and how often (for example,
  *"patrol my deals every 2 hours and update the dashboard if pacing drops"* or
  *"check back in 30 minutes and verify the backfill"*). The assistant uses the
  `chat_patrol` tool (`create`, `list`, `update`, `pause`, `resume`, `cancel`) to
  attach or adjust the schedule on the current conversation.
- **Use Patrols & timers in the composer:** open **More options** (`•••`) in
  the composer toolbar and click **Patrol & schedule** (or, once the chat has a
  patrol, click the **clock** on the toolbar). In a chat with no patrol yet,
  **Patrols & timers** opens straight on the **New patrol or timer** form;
  otherwise press **New patrol or timer** under the list. Describe what to check
  and do each run, pick a schedule under **Repeats** (**Every 1h**, **Every
  2h**, **Every 4h**, **Every 6h**, **Weekdays 9am**, **Daily 9am**, or
  **Custom** to build a recurring schedule by hours, minutes,
  daily/weekday/specific days of the week and time zone — with an **Advanced
  cron** option) or under **Once (timer)** (**In 30m**, **In 2h**, or **Custom**
  in minutes or hours), choose the approval mode, optionally give it a label,
  and press **Start patrol** (or **Set timer**).

When a conversation has an active or paused patrol or timer, the **Patrol bar**
appears above the composer showing each one's name, cadence, countdown to the
next run (or **Paused**), and run count, with an **Approval mode** dropdown,
**Run now**, **Pause / Resume**, and a **Hide** button (the down arrow at the
right). **Hide** only folds the bar away; the patrol keeps running. The clock on
the composer toolbar gets a ring, and a **Patrol bar hidden** note with **Undo**
shows for a few seconds. This browser remembers the choice for that chat. A
hidden bar comes back on its own while an approval card is waiting, with a
**Review** button that takes you to the card (the clock turns amber too).

Click the clock to open **Patrols & timers**, which lists every patrol and timer
in the chat with **Run now** and **Pause / Resume**. Expand one to change the
approval mode, read what it does each run, **Edit schedule & task**, or
**Remove patrol** (**Cancel timer** for a timer); removing takes a second click
to confirm, so a patrol is never stopped by one click. **Show bar above
composer** brings a hidden bar back. The approval mode belongs to the chat, so
it applies to every patrol in it, and switching a running patrol to
**Autonomous** from the bar asks you to confirm first.
Choosing **⚡ Auto-approve** (in Patrols & timers, the Patrol bar, or **Approval
mode** under **More options** in the composer) lets session-approvable tool
calls run hands-free while you are away, while **Autonomous** (after you confirm
the Autonomous risk prompt) runs bundle-configured live platform actions, optional
connector activations, and self-addressed owner emails without pausing and
leaves durable receipt cards in the transcript (external emails/chats/invites,
scheduled-task and patrol schedule changes, destructive storage or permission
changes, bundle-blocked actions in `agent_policy.critical_tool_autonomous_blocked`,
and `git push` still ask, and only turns triggered by the conversation owner use
Auto-approve or Autonomous).
If a patrol turn does stage an approval card, the card stays open for 24 hours
and automatically resumes the patrol turn as soon as you approve it.
While the card waits on you, the patrol's scheduled runs are skipped rather
than stacked up behind it, and the Patrol bar says so under that patrol:
**Patrol on hold**, with the times of the runs it skipped. Approve or decline
the card and the next scheduled run goes ahead.
Conversations with an active or paused patrol are exempt from retention sweeps
until the patrol is stopped.

A patrol runs as the chat's persona, so a persona of your own — a digital twin,
say — can keep watch for you: press **Start patrol** on one of its suggested
patrols in **Settings → Personas** (see
[Your own personas and digital twin](#your-own-personas-and-digital-twin)).

### ACP clients (Zed, Neovim, Emacs, JetBrains, Buzz)

`fleet acp` lets an Agent Client Protocol client talk to fleet. The client
launches it, and each message becomes a normal fleet chat turn as the user named by
`--email` or `FLEET_USER_EMAIL`. The conversation appears in the web chat like
any other. Replies, reasoning and tool steps stream to the client. If a step
needs approval, the reply ends with a link or a `fleet chat --approve` command,
because approvals are settled in fleet, not in the client. Images and audio are
not accepted. JetBrains sends a screenshot you drop into the chat as its file
name only, so the assistant never sees the image, and nothing warns you.

Files you attach in your editor reach fleet as the editor sends them. When it
sends only a link to a file, as JetBrains does for a file you attach and for the
file open in the editor, `fleet acp` asks the editor for the file's text, if the
editor lets agents read files (JetBrains and Emacs agent-shell do), and sends
that with your message. Up to 100 KB of a file, and 300 KB of all the files a
message links to, are sent: a longer file is cut, and one over 1 MB is not sent
at all; either way the assistant is told. In JetBrains this means the file open
in the editor goes to fleet with every message, each time adding its text to the
conversation, so close a file holding secrets before you chat.

If your operator has added fleet to your Buzz workspace, you @mention it
there. By default only the person who set it up, and their own agents, can
instruct it; your operator can let others too. Its reply comes once its turn
ends, in the thread of your message,
and @mentions you: nothing appears while it works, and replies are not
streamed or edited. If you send fleet another message while it works, it
answers both in one reply. Files attached in Buzz cannot be read by fleet,
and Buzz's agent instructions and memories do not reach it. The conversation
is also in fleet's web chat, under the user the agent runs as.

MCP servers your editor sends are not used by fleet: the ones you set up and,
unless it is turned off, the one built into JetBrains. fleet's tools and
connectors are the ones your operator set up, and they run on the fleet server.
The first reply in a session says so, naming the servers your editor sent.

When the client opens a session, `fleet acp` checks your user and token with the
fleet server first. A wrong token, an email that is not a fleet user, a
read-only viewer, or an address the server does not admit stops the session
from opening, with an error that says which and how to fix it. A server that
cannot be reached does not: the session opens, and your first message reports
the problem.

A reply ends in the client as soon as its answer is complete. fleet may still
need a few seconds to finish the turn (by default it names a new conversation
after its first reply); a message you send in that time waits, for up to 90
seconds, rather than being queued.

A message sent from the client while an earlier one is still running does not
stop it. It waits, and messages on the same session run one at a time, in the
order the client sent them. A cancel from the client stops the reply running
and the messages sent before the cancel that are still waiting; a message sent
right after the cancel still runs. `fleet acp` waits up to 2 seconds for a
message that is slow to reach it to keep that order, then goes on without it,
so in that rare case a cancel can miss the message sent just before it. Up to
20 can wait behind the one running. A
21st is refused straight away with an error saying the session already has a
prompt running and 20 waiting. It is not sent to fleet, so send it again once
the earlier ones have been answered, or cancel them from the client.

If you close the editor, or stop the agent, while a reply is still running, and
the editor lets the agent shut down (Neovim's CodeCompanion.nvim does), that
turn is stopped in fleet too. Messages from the client still waiting behind it
are dropped: they never reach fleet. A message fleet itself had already queued
(behind a turn started from the web chat, say) still runs. An editor that
force-kills its agent leaves the turn running: stop it from the web chat. Zed
does that when it quits; in Emacs, quitting Emacs stops the turn, but killing
the agent-shell buffer does not.

### What happens when you ask

The assistant does the work rather than describing it. It has a private working
space for each conversation with a shell, a Python runtime, and file tools, so it
can open the data you have given it access to — attached, connected, or shared —
work through it, and hand you the result in whatever form you asked for. Every
step it takes is visible in the transcript as a trail you can expand. Anything
with consequences outside the conversation, such as sending an email or
scheduling a task, stops and asks you first.

That working space is a **sandbox**, and it is not optional: every command, every
line of Python, and every file the assistant reads or writes runs inside it,
isolated from the machine the platform runs on. Credentials for connected systems never
enter it.

## 2. Reading the screen

Three regions: the rail on the left holds your conversations, the transcript in
the middle holds the work, and the composer at the bottom is where you ask.

### The rail

The rail is one sidebar, and it is the same on every page: Chat, Patrols,
Background tasks, Settings, Guides and Analytics. Wherever you are, one click
on a chat in it takes you back to that chat. Everything between the search
field and your account menu scrolls together, as one list.

At the top are your deployment's logo and name, a gear that opens a short
[Customize](#customizing-your-sidebar) panel, and a button that collapses the
rail to a strip of icons. Under them:

- **Search**, also reachable with `⌘K` or `Ctrl K`, searches everything in
  the rail at once (see **Searching the rail**, below the table).
- The **pen** starts a new chat.
- The **+** opens a menu: **New chat**, **New patrol** (the
  [Patrol Launchpad](#patrol-launchpad-your-first-patrol-in-five-steps)),
  **New background task**, **New project** and, where your deployment offers
  it, **New sealed chat** (a chat in Lockdown mode).

Next come the top items, which stay in view above your lists: **Patrols**
(the [Patrols page](#the-patrols-page)), **Background tasks** (formerly the
Operations Center) and anything you choose to keep there, such as a
dashboard (see [Customizing your sidebar](#customizing-your-sidebar)). Below
them are your lists:

| List | What it holds |
| --- | --- |
| **Pinned** | What you have chosen to keep at hand: your pinned projects and pinned chats, and any teammate's chat you pinned from its menu, in one list. Your own pinned conversations never expire. With two or more entries, **Reorder** in the heading lets you put them in your own order (see [Ordering your chats](#ordering-your-chats)). |
| **Projects** | Workspaces with standing instructions, shared learnings, and their own conversations. A project shared with your team carries a team glyph. The first eight are listed; **All N projects** shows the rest and **Show fewer** folds them again. Click a project's name to open its page; the arrow at its left (shown when you point at the row, and always on a touch screen) shows or hides the project's chats in the sidebar; `⋮` opens its menu. A pinned project is listed in **Pinned** instead. See [Memory and projects](#7-memory-and-projects). |
| **Chats** | Your other chats and, by default, your teammates' shared chats, in one list, most recently active first, under the date headings **Today**, **Yesterday**, **This week** and **Older**. When the list holds teammates' chats or patrol chats, chips above it narrow it to **All**, **Mine**, **Shared** or **Patrols**; a chip appears only when it would change the list. The menu beside the heading holds **Sort** (**Recent activity**, **Newest first** or **Title A–Z**; an A–Z list has no date headings), **Filter by label**, **Select chats…** and **Delete temporary chats…**, which deletes all your temporary chats at once after you confirm. A chat of yours that is not pinned, in a project, shared or on patrol is temporary: it is deleted after a period without activity (90 days by default). For the last part of that period (14 days by default) its row shows the days it has left, and a line under the heading counts the chats about to be deleted; click it to go to the first. Sending a message in a chat starts its period again. Pin a chat, move it into a project or archive it to keep it. |
| **Team chats** | Conversations shared with you or open in **Multiplayer mode** from teammates. By default they are in **Chats**, among your own, each with its owner's initial; [Customize](#customizing-your-sidebar) can give them a **Team chats** list of their own, or hide them. Each row's `⋮` menu holds **Pin**, **Move to the top** and **Set as start page** (none of them on a read-only **Viewer** account) and **Copy link**. **Pin** moves the chat into your **Pinned** list, for you alone: nothing changes for its owner or for anyone else, and it does not stop the owner's chat from expiring. **Copy link** copies: for a chat shared with your team, the team link, which opens it for anyone on your team; for a chat you were added to in Multiplayer, its chat link, which opens it for the people in it. A Multiplayer chat you take part in also has **Download chat…**. Renaming, filing, archiving and deleting stay with the chat's owner. |
| **Archived** | At the foot of **Chats**, once you have archived a chat, and collapsed by default. Conversations you have put away but not deleted; open it to bring one back. |

**Searching the rail.** Type in the search field and the groups above step aside for one list of
results. Search looks through all of your chats (pinned, temporary, and filed
in a project), your teammates' **Team chats**, your **Projects**, and your
**Archived** chats, whichever lists you have folded or hidden in your sidebar.
Clear the field and the rail returns exactly as you left it.

- Every word you type must appear in the name, in any order; capitals and
  accents do not matter. The matched words are highlighted.
- Results are grouped by where they live — **Chats**, **Team chats**,
  **Projects**, **Archived** — each with a count. A chat in a project shows
  the project's name before its title, and a patrol chat carries its patrol
  mark.
- A teammate's chat is also found by its owner: type a colleague's name to
  see the chats they shared.
- **In messages** lists chats where the words appear in the conversation
  itself, with the matching line beneath. This covers your own chats,
  archived ones included (tagged **Archived**), and the chats shared with
  you: your teammates' chats, marked with their owner's initial. In a shared
  chat it searches what was asked and answered. A deployment can switch
  message search off, in which case this group does not appear.
- If a result comes from a list you have hidden, the group says **Hidden in
  your sidebar** and offers **Show it**, which puts that list back in your
  sidebar (not offered on a read-only **Viewer** account).
- From the field, `↓` moves into the results, `↑` and `↓` move between them,
  and `Enter` opens the first one.
- When nothing matches, the rail says so and **Clear search** empties the
  field.

A row is its title, with at most one status at its right edge, shown only
when it matters and explained when you hover. The first of these that applies
is the one shown: a pulsing dot while a reply is being written (in Chat only: the sidebar on other pages does not show it); the days left
on a temporary chat that is about to be deleted; a pulse line on a **patrol**
chat, with the time until its next check (or **Paused**); a green dot on a
**Multiplayer** chat; a team glyph on a chat
shared with your team; a link on a chat shared by link; a lock on a sealed
chat. A teammate's chat also carries its owner's initial. A chat with a patrol
is kept, not deleted for inactivity. A temporary chat shows nothing until it
is close to being deleted, and labels are not drawn on rows: a chat's labels
are in its tooltip and under **Labels** in its menu, and **Filter by label**
in the Chats menu shows only the chats that carry them. Click a project's
name to open its home (and expand it); the chevron only folds it. Hovering a
project shows its menu: **Open project**, **New chat**, **Reorder chats**
(when it holds two or more of your chats), **Move to the top** and **Set as
start page**, and, for the owner, **Pin**, **Rename**, **Share…**, **Project
settings…** and **Delete project**. See [Keeping, shaping, and sharing conversations](#8-keeping-shaping-and-sharing-conversations).

Drag the rail's right edge to make it wider or narrower, and double-click the
edge to go back to the default width. From the keyboard, move the focus to the
edge and press the left and right arrow keys, `Home` and `End` for the
narrowest and widest, or `Enter` for the default. The width is remembered in
this browser. Collapsed, the rail is a strip that keeps the logo, a button to
expand it again, the **+** menu, the top items and your account menu. On a
phone the rail is a drawer, opened with the menu button at the top left of
each page.

Your account menu at the foot of the rail switches between **Light**, **Dark**,
and **System** appearance and opens **Settings**, **Guides** (these user
guides) and **Context & Connections**; administrators also find **Admin
Settings** and **Analytics** there. Its **Sign out** asks you to
confirm first, because it also signs you out of every app that shares your
sign-in.

A link to a Fleet page (a chat, a new chat with a message already in the
composer, a task) that you open while signed out, or after your session has
expired, takes you to sign in first and then on to where it pointed. That
works for 30 minutes after you open the link, in the same browser. After that,
or in another browser (a sign-in email opened somewhere else, say), you land
on your usual start page. If you open another such link before you sign in,
or a Fleet page once you are signed in, it replaces the earlier one. A link
longer than about 2,000 characters also lands on the start page, except the
link for your team, which still opens its chat without the rest of the link.
A message a link puts in the composer is never sent until you press Send.

**Settings → General → Language** selects **English**, **Español**, or
**Português** for the interface and these guides. Your choice is saved in this
browser. Before you choose, the platform uses your browser's language when it
is supported, otherwise English. Changing the language reloads the current
page and keeps its path and section; the URL's `lang` parameter also preserves
the choice when browser storage is unavailable. Conversations, files, prompts
you saved, and other content you wrote stay in their original language.

**Settings → General → Browser notifications** turns on alerts in the browser
you are using, even while the tab is in the background. By default you get one
when a task you created finishes, is waiting for your answer, or sends a
progress note while it runs (**Progress note**: the task carries on, and the
note itself is not shown), when an action in one of your chats needs your
approval, and, if your account has a usage budget, when you try to create a
task while your spending is past its soft limit (**⚠ Budget alert**: it is
checked then, not the moment spending crosses the limit);
[Cards that ask for a decision](#6-cards-that-ask-for-a-decision) describes the
others. The browser asks for permission the first time. An alert shows only a
short title (the start of the task's prompt and how it went, the action
waiting on you, or the budget and its window), never the conversation or a
result. The switch is per browser and per account, so turn it on in each
browser you want alerts in. A **Viewer** account cannot turn them on: the
switch is greyed out and the line under it says so (if they were on before the
account became a Viewer, it can still turn them off). They work in Chrome,
Edge, Firefox and Safari, and in browsers that use the same push services as
those; in a browser that uses a different push service, the switch reports an
error, so turn them on in one of those browsers instead. When the switch is
greyed out for any other reason, the line under it says why: the browser does
not support them, they are blocked for this site in the browser's own settings
(allow them there, then reload), or your deployment has not set them up. If
your deployment changes its notification keys, a browser gets no alerts until
you next open Fleet in it, which renews them by itself; if the switch then says
they could not be renewed, reload the page.

Administrators manage account access under **Settings → Admin → Users**. Chat
defaults to **Contributor**; **Viewer** is read-only: a Viewer's sidebar lists, searches and opens chats and projects but offers nothing to create, pin, rename, label, move, share, archive or delete. **Fleet Admin** grants full
Chat and Background tasks permissions and highlights the included Contributor choices.
Selecting Fleet Admin again turns it off and safely defaults to Chat Contributor
with no Background tasks access. A person has one Fleet account: Chat and
Background tasks share its email, password, and Fleet Admin role, and its
Background tasks access is part of the same account. Accounts created and granted Fleet in central Auth are added
here automatically with the roles selected there. When your deployment signs
people in through central Auth, central Auth manages everyone by default: the
sign-in page offers single sign-on only, and this page lists people and their
usage without any controls (no **Add user**, no row editor, no **Rename team**)
and links to central Auth, where people are added and removed and their roles,
teams and passwords are changed. **Settings → Team** then shows your team but
cannot change it. Moving someone between teams in central Auth moves them
here too, with the same effect as moving them yourself: what they shared with
their old team stops being shared. Renaming a team in central Auth renames it
here and keeps everything shared with it shared with the renamed team.
Otherwise, renaming a team here keeps everything shared with it shared with
the renamed team; a new team that later takes the old name starts with
nothing shared.
**Delete user** in a person's row removes their account with their chats,
memories, connected MCP servers, connector settings, skills, personal
projects, API keys and uploaded files. When the account is deleted but a step
does not finish (their API keys could not be removed, say), the page says so
in a notice above the table, with what is left and how to remove it.

### Customizing your sidebar

The rail is yours to arrange. The **gear** at the top of the rail opens a
short panel with the choices people change most:

- **Fleet opens to** picks your start page: **The last chat you had open**
  (the default), **Background tasks**, anything you keep at the top of the
  rail, or anything you have pinned.
- **Density** switches the rows between **Comfortable** and **Compact**.
- **Show** turns **Patrols**, **Background tasks**, **Projects** and
  **Teammates' shared chats** on or off.

**More options** in that panel, or **Customize sidebar** in your account
menu, opens the full panel. It slides in from the right and leaves the rail in
view, so every change shows in it as you make it. Changes are saved to your
account a moment later, so your sidebar is the same in every browser you sign
in to. A read-only **Viewer** account can open both panels but not change
anything.

- **Fleet opens to** is the same choice as in the short panel. Your start
  page is where fleet takes you when you open it without a link to something
  else: a dashboard, a chat, a project, Background tasks or a page. Moving
  around inside fleet never sends you back there, and a link to a chat still
  opens that chat. With no start page set, fleet opens the last chat you had
  open.
- **Start from** offers five arrangements: **Classic** (Patrols and
  Background tasks, then Pinned, Projects and Chats), **Just chat** (your
  chats and pins, nothing else), **Dashboards first** (what you keep at the
  top comes before Patrols and Background tasks), **Automation** (Background
  tasks and Patrols lead) and **Team room** (your teammates' chats in a list
  of their own, first). Choosing one rearranges the rail; your pins and your
  density stay as they are, and so does your start page unless the
  arrangement hides it.
- **Top of the sidebar** lists Patrols, Background tasks and whatever you
  keep there. **Add** adds a chat, a patrol, a teammate's shared or
  multiplayer chat, a project, a recent background task, or a page (Guides,
  Settings, and Analytics for administrators). Drag a row, or use its arrows,
  to move it; type over its name to rename it; press its icon to pick another
  icon or any emoji; switch it off to hide it; press **×** to remove it;
  press its map pin to make it your start page. A chat can open as a **Chat**
  or as a **Dashboard**, straight into [Dashboard view](#dashboard-view).
- **Pinned** lists your pinned chats and projects in their order: use the
  arrows to reorder them, the bin to unpin one, and **Pin** to pin another
  chat or project.
- **Lists** orders, renames, hides or folds **Pinned**, **Projects** and
  **Chats**. Search still finds what is in a hidden or folded list.
- **Teammates' shared chats** puts your teammates' chats **In Chats** (mixed
  with your own; the default), in an **Own list** (a **Team chats** list you
  can place above or below your chats), or **Hidden** (not listed; search
  still finds them).
- **Group chats by date** turns the **Today**, **Yesterday**, **This week**
  and **Older** headings in Chats on or off, and **Status on rows** does the
  same for the status at the right of each row.
- **Density** is **Comfortable** or **Compact**, as in the short panel.

**Move to the top**, in the menu of a chat, a project or a teammate's chat,
keeps it among the top items without opening the panel (a chat you last read
as a dashboard is kept as a dashboard). It then leaves the list it was in,
and the same item reads **Remove from the top**. **Set as start page**, in
the same menus, makes that chat, dashboard or project the page fleet opens
to; it does not have to be a top item, and the item then reads **Remove as
start page**. **Reset to default** in the panel returns you to your
workspace's sidebar. A chat kept at the top that you can no longer open is
shown faded, so you can remove it.

Administrators set the sidebar everyone starts with under **Settings → Admin
→ Sidebar** (the **Workspace default** tab at the top of the full panel goes
there): the same editor, without the Pinned list, saved with **Save for
everyone**. Its **Add** list offers only chats and projects shared with your
team (your own sidebar can keep private ones too), and one that is not shared
is marked **Not shared with the team**. People who
have not customized their own sidebar get it the next time they load a page,
and **Reset to default** brings anyone back to it. A chat kept there shows
only to people who can open it, which makes a shared or multiplayer chat (a
team-wide patrol, say) a good start page for everyone; a start page someone
cannot open is skipped for them. Background tasks cannot be added there,
because only their owner and administrators can open one. **Start from my
sidebar** copies your own arrangement as a starting point, and **Reset to
fleet's default** removes the workspace sidebar.

### The header

Above the transcript sit two quick desktop controls on the right: **Dashboard**
switches the conversation to [dashboard view](#dashboard-view), and **Keyboard
shortcuts** shows the same list as the [last section](#13-keyboard-shortcuts) of
this guide. On narrow mobile screens (`< 40rem`), the
**Dashboard** and **Keyboard shortcuts** buttons are hidden from the header to keep
the top bar compact (switch to dashboard view via **More options → Dashboard view**).
**Memories** and **Show details** (which toggles the assistant's reasoning, working notes, tool calls, and per-turn cost figures on or off across the conversation) live inside **More options** (`•••`) in the composer so they carry clear labels and stay reachable in both standard chat and dashboard view.
When your operator configures a support
contact in the deployment's bundle (`branding.support`), the top bar also shows
that support button across Chat, Background tasks, Settings, and Guides
(collapsing to a compact **Help** label — or `branding.support.mobile_label` — on
mobile screens);
pressing it opens a dialog with your operator's message, a **Copy email** button,
and a **Send email** (`mailto:`) action (omitted by default when not configured).
Every conversation also shows who can see it — **Only
you**, **Shared with your team**, **Multiplayer**, or **Public link** — in the header, and **Share this chat** in **More options** (`•••`) in the composer (plus an active sharing indicator on the composer toolbar whenever the chat is shared).
Both open the share dialog.

### The transcript

Your own messages appear as you typed them; very long messages (over 4,000
characters) collapse to a preview by default with a **Show full message** /
**Show less** toggle underneath. When the platform adds something to a
message that you did not type, such as an attached file or a note from the shared
file library, it appears as a collapsed note beside your message rather than
inside it, so what you wrote and what was added stay visibly separate.

Each reply from the assistant is laid out in a fixed order, top to bottom, so
your eye lands on the answer and then on anything that needs you:

| Part | What it is |
| --- | --- |
| Execution trail | A row of chips, one per tool the assistant used: a file it read, a connector it called, code it ran. Click a chip to see exactly what went in and what came back. This is how you check the work. A chip in the danger colour is a call that failed; the chip of an action that went to an approval card is not: it is marked **sent for approval**, and the card says whether it ran. Shown only when **Show details** is on (for turns run under another person's account in a multiplayer or shared chat, **Turn details hidden for other users' turns** appears instead of their private tool calls, reasoning, and per-turn cost figures). |
| Reasoning | The assistant's narrated thinking. Also shown only when **Show details** is on. |
| Working notes | While a reply is in progress, the short notes the assistant writes between steps ("Let me look up the deal first."). Shown only when **Show details** is on, and only until you reload the page: the saved conversation keeps the answer, not the notes. With details off, a status line takes their place: **Thinking**, or the step under way, such as **Running shell** or the plan item it is working on. |
| The answer | Text, tables, charts, and images. Code and data come in copyable blocks. When the assistant produces an HTML report, it renders as an **HTML preview** with a **Show source** toggle. |
| Interactive cards | A form, picker, checklist, comparison, chart or small calculator the assistant built for this answer. Fill it in and press its button; your answers go back as your next message. See [Interactive cards](#interactive-cards). |
| Status | Banners for anything unusual about the turn: retrying, cancelled, failed. See [When a turn goes wrong](#11-when-a-turn-goes-wrong). |
| Cards | Anything asking for your decision: an approval, a proposed memory, a suggested model switch. See [Cards that ask for a decision](#6-cards-that-ask-for-a-decision). |
| Footer | The actions on a finished reply: **Copy**, **Regenerate** and **Retry with** the stronger model (both under the latest reply only; **Retry with** when the chat is not already on it and no switch card is waiting, since the card offers the same), **Branch**, **Save as workflow**, and **Save to memory** (the answer, as a memory). On a phone, or in a window too narrow to show them all on one line, **Copy** and **Regenerate** stay in view and the rest open from the **•••** (more actions) button beside them, each with a line saying what it does. Each is covered where it belongs later in the guide. |

Long conversations collapse their older turns behind **Show N earlier turns**,
and a minimap along the edge lets you jump between messages (it is hidden in
dashboard view).

### The composer

The toolbar beneath the message box keeps everyday controls upfront and groups secondary options into **More options** (`•••`). Left to right:

| Control | What it does |
| --- | --- |
| Model selector | Shows the model this conversation runs on, with a credit-usage meter (one to four pips, green for low through red for very high) where its pricing is known. Click to change it. See [Picking the model](#picking-the-model). |
| Attach files | Add files to your next message. Dragging a file anywhere onto the conversation does the same. |
| Tools | The wrench. Its badge counts the connectors on in this conversation, **Always on** ones included; hover it for "Tools, N on". See [Connectors in a conversation](#5-connectors-in-a-conversation). |
| More options (`•••`) | Opens a labeled menu organized into three groups: **Prompt library**, **Email templates** (only when your deployment has any), **Persona** (active on any new or empty chat before the first message is sent, and shown disabled with *"Personas can only be changed in a new chat"* once chatting has started), and **Memories**; **Share this chat**, **Patrol & schedule**, **Approval mode** (**Manual**, **Auto-approve**, or **Autonomous**), and **Summarize earlier messages** (once a conversation has messages, showing how full the chat is); and **Show details**, **Dashboard view**, and **Send on Enter**. When a non-default persona, a patrol schedule, sharing (**Multiplayer**, team sharing, or a public link), **Auto-approve**, or **Autonomous** is active, **More options** shows an accent dot, and on desktop screens a quick status pill or indicator also appears directly on the toolbar so you can see or change it in one click. |
| Send | At the far right. While the assistant is working it becomes **Stop**. |

### Dashboard view

Dashboard view turns a conversation into a live board: the assistant's charts,
numbers and tables stay on screen as panels, and the chat narrows to a rail
beside them. Use it for anything you want to keep an eye on while you keep
talking: a pipeline, a campaign, a launch checklist, a comparison you are
refining. Turn it on with **Dashboard** at the top of the chat (on desktop screens), **Dashboard
view** in **More options**, or `D`; leave with **Exit**, or `Esc` when you are
not typing in the message box. The rail stays beside the board as its strip
of icons, so the top items and the **+** menu are one click away. The expand
button at the top of the strip slides the whole sidebar in over the board, so
you can move to another dashboard or chat without leaving the view; `Esc`, or
a click outside it, closes it again and leaves you on the board. On a phone,
the sidebar button at the left of the board's header opens the sidebar. A chat
[kept at the top of your sidebar](#customizing-your-sidebar) as a dashboard
opens straight into this view.

- **Ask for what you want to watch.** "Turn this into a dashboard", "add a
  panel for open deals", "refresh the revenue panel". The assistant builds
  panels from the same [interactive cards](#interactive-cards) it uses in chat,
  and updates a panel in place when you ask it to refresh, so the panel keeps
  its spot and shows a small version number (**v2**, **v3**) and a brief glow.
  Messages you send from dashboard view tell the assistant you are looking at
  the board and which panels are on it, in order, so "update this chart",
  "refresh the second panel" or "add last week to the revenue panel" reach the
  right one, including a card you pinned yourself. They also ask it to update
  panels rather than repeat their numbers in the chat.
  An empty board offers a few starter requests; picking one puts it in the
  message box for you to edit and send.
- **What goes on the board.** Cards that only show something (charts, figures,
  tables, checklists) go on the board, and so does any card the assistant
  marked as a panel. Forms that ask you a question stay in the chat, where the
  conversation is; **Pin to board** under a form adds it as a panel. A card on
  the board shows in the chat as a one-line pointer; click it to jump to the
  panel.
- **Arrange it.** Drag a panel by the handle at the left of its title, or focus
  the handle and use the arrow keys. The buttons on a panel widen or narrow it,
  **Focus** it to fill the board (`Esc` or the close button returns), and
  **•••** moves it or takes it off the board; its card then goes back to the
  chat. The layout, and whether a conversation opens in dashboard view, are
  remembered in this browser only, so a teammate opening the same chat sees the
  panels in their own arrangement.
- **Keep it current with a patrol, and share it with your team.** An
  [in-chat patrol](#in-chat-patrols-and-timers) can refresh the board on a
  schedule: ask for it in the same chat ("patrol this every morning and update
  the panels"), and each run redraws the panels it has new figures for. In a
  [Multiplayer](#sharing-with-your-team) chat everyone sees the same panels,
  whoever's message or patrol drew them, and anyone in the chat can ask for a
  panel to be updated or added: the assistant works from the whole chat,
  including what it did on a teammate's turns, so it changes the existing
  panel instead of adding a second one.
- **Nothing changes about what the assistant can do.** You type, attach files,
  and answer approval cards in the chat rail exactly as before, and emails and
  other actions still wait for your approval. The status light at the top says
  **Standby**, names the step in progress while the assistant works, or shows
  how many approvals are waiting; click it to jump to the first one.
- **On a phone**, the top bar has **Board** and **Chat** buttons to switch
  between the two.
- **The conversation list is out of sight**, so its shortcuts (`J`/`K`, `P`,
  `A`, `R`, `#`) do nothing in dashboard view, and `⌘ K` leaves dashboard view
  to open search.
- **Switch back any time.** Leaving dashboard view returns the normal chat with
  every card in place and whatever you had typed, in the message box or in a
  card, still there. Coming back restores the board as you left it.

Panels are cards, so the limits under [Interactive cards](#interactive-cards)
apply: a panel from before a compaction is marked **archived** and can't be
answered, and the board appears only in the web app.

## 3. Getting good answers

If a provider fails after a completed tool step, the assistant can continue
with that step's results through bounded recovery. A tool begun during the failed
step still stops recovery to avoid repeating an uncertain action. Check the
execution trail before manually repeating a request that ended with an error.

Brief the assistant the way you would brief a capable colleague: say what you
want, name what it should use, and react to what comes back. The quality of the
answer follows the quality of the ask, and a few habits raise it every time.

| Habit | Why it works |
| --- | --- |
| **Name the source** | Say which report, which sender, which connector. "The most recent daily delivery report in the mailbox" works; "the latest file" leaves the assistant guessing, and a wrong guess can look very much like a right one. |
| **Name the identifiers** | Deal names, campaign codes, and IDs are the things most likely to be misread, even when every number around them is correct. Spell out exactly which identifiers you want, and ask for them to be shown as they appear in the source. |
| **Anchor on the data** | Reports trail the calendar by a day or more, so ask for "the latest date in the file" rather than "yesterday". Each source has its own latest date; let the assistant find them from the data instead of assuming. |
| **Ask it to show its work** | "Show me the rows behind that number" is the fastest check there is. The execution trail shows what it did; this shows you the evidence. |
| **Flag, don't guess** | Tell it plainly: "if something cannot be opened or is missing, say so and continue with the rest." A report you can trust is one that tells you what it could not do. |
| **Small asks, one at a time** | Build in steps. Get the data right, then the comparison, then the format. React to each result: "too much detail", "show that as a table", "put the dates up top". The correction conversation is how a report reaches its final shape. |

### Picking the model

Every conversation runs on a model you can change at any point, including
mid-conversation. Click the model selector to open the picker; type to search,
and pick the one that fits the job. Models your deployment marks **recommended**
sit at the top of the list. The first is the **default model** — our smartest
frontier model, which every new conversation starts on so you get the best
reasoning and tool use out of the box — and the second is the **fallback model**,
another smart frontier model from a different provider family that tasks and
patrols fail over to automatically and that the assistant offers to switch to if
a turn struggles. By default those two are **Gemini 4 Hydrogen** (new conversations
start on it) and **Claude Opus 5.5** (the frontier fallback model); your admin
can point either slot at a different model, and when your admin upgrades the
workspace's primary or fallback model, existing conversations and recurring
tasks can move to the new model automatically with a short note in active chats. When you want to optimize your spend
on routine pulls, checks, or formatting, you can switch to a cheaper or free
model in the picker. Your admin can also pin models just below the two
recommended frontier models, sometimes under their own name and with a tag such
as **alpha** that marks a model still in trial.
A pinned model can also show **Free** where its credit-usage meter would be. The
rest of the list is marked by status (**tested**, **new**, or **experimental**),
and a model whose pricing the deployment knows shows a credit-usage meter: four
pips, of which one to four are filled, coloured green for low usage through
amber to red for very high. Hover it for the model's blended price in credits
per million tokens, so you can see what you are about to spend. Costs are shown
in credits throughout fleet. No meter means the price is unknown rather than zero
— worth asking about before running something long on it.

The picker uses your workspace's active model providers, including providers
configured by the deployment. **Workspace** models route directly through their
named provider. Recommended and public-catalog choices that the workspace cannot
route are hidden once provider information loads. A saved conversation or default
that is no longer available stays visible with an explanation: use **Choose a
model**, pick an available workspace model, then retry. Fleet does not silently
switch an existing conversation to another provider, with one exception: a
**lockdown** conversation may only run on the models your admin allows for
lockdown (by default the workspace's default and fallback models), so if that
list changes under it, its next turn runs on the lockdown default and the
conversation keeps that model from then on. The model picker shows the switch
when the turn starts. That switch needs a specific model to move to: if your
admin has written the lockdown list entirely as patterns rather than named
models, there is nothing to switch to and the conversation keeps refusing —
start a new lockdown chat and tell your admin the list needs a named model
first. Admins can fix the provider
configuration under **Settings → Admin → Model providers**, and set the default
and fallback model under **Settings → Admin → Features → Model tiers**. Besides
OpenRouter, Anthropic, OpenAI and Ollama, that page can add any
**OpenAI-compatible** server (it needs its Base URL; a key is optional), the
**Vercel AI Gateway** (it needs a key), an **Azure OpenAI** resource (it
needs a key, and the resource's endpoint as its Base URL; its models are the
resource's deployment names), **Google Gemini** (it needs a Gemini API key
from Google AI Studio) and **Amazon Bedrock** (Claude models only; it needs a
Bedrock API key and the AWS Region the key was made in, us-east-1 by default,
and its models are Bedrock inference profile ids such as
`us.anthropic.claude-sonnet-4-5-20250929-v1:0`). On Azure, a GPT-5 or o-series deployment needs its
model in its name, such as `gpt-5-prod`, or its turns fail, and a newer
reasoning model such as GPT-6 may fail whatever its deployment is called. A
Vercel model listed there takes that
model over from OpenRouter, so it then runs through Vercel. A
provider added there, other than OpenRouter, needs its **Context window** (in
tokens, the smallest limit among the models it serves) before it can be
enabled. One without it is marked **No context window**, and a turn on its
models usually stops before any reply with "The selected model could not
complete this turn": if that happens every time on one workspace model, ask
your admin to set it. A task you
schedule from chat does not keep the chat's model: unless you ask for a specific
model, it runs on the workspace's task model, which is the stronger model by
default.
If a direct provider takes precedence over OpenRouter, available OpenRouter
choices are labeled with their configured provider name and use an explicit
provider-prefixed route.

The assistant will sometimes suggest a switch itself: a card offering **Switch &
retry** when a question is heavier than the current model handles well, or a
one-line banner when you attach a large spreadsheet to the fast model. Fleet can
show the same card on its own after a reply that clearly struggled (several tool
calls failing in a row, the same step repeated, or a very long turn) and, if your
admin hasn't turned it off, when your message reads as pushback on a wrong answer.
All of these are suggestions. Nothing changes unless you accept. If an answer is
wrong and you don't want to wait for a suggestion, **Retry with** under the latest
reply asks again on the stronger model straight away.

### Personas and skills

A **persona** is a saved working style your deployment may ship, chosen from the
composer. A **skill** is a packaged capability the assistant can pick up on its
own when a task fits, or that you can invoke directly by typing `/` at the very
start of a message and choosing from the list. The platform ships a small pack
of general-purpose skills, and your deployment can add its own. Under
**Settings → Skills**, you can also create personal skills, publish a skill to
**Workspace skills** (**Share with your workspace**), click **Add to my skills**
on a teammate-shared skill in Workspace skills to enable it for your own chats
and tasks, or click **Clone & edit** on any workspace skill to load its
instructions into the editor and customize it (you can also ask the assistant in
chat to propose a skill or edit an existing one, then click **Approve** or
**Approve & share** in **Settings → Skills**). Your active personal skills appear
alongside the workspace's skills in the composer's `/` autocomplete menu (and if
you customize a workspace skill under the same name, your version overrides the
default description and instructions for your runs).

### Your own personas and digital twin

Under **Settings → Personas** you can write personas of your own. They are
private: only chats you start can use them, and they appear under **Yours** in
the composer's persona menu (inside **More options** `•••`).

- **Build your digital twin.** Describe yourself (your role, what you watch, how
  you decide, how you like things written), choose whether the twin may read
  what you typed in your recent chats, and choose **Suggest actions** or **Act on
  my behalf**. **Build with the assistant** opens a new chat with that request
  already in the composer; nothing is sent until you press Send. The assistant
  can read only the messages you typed — not its own replies, and never
  lockdown, archived or deleted chats — asks a few questions, then stages a
  **draft** persona with suggested patrols. **Write it myself** opens the editor
  on a twin template instead.
- **Drafts wait for you.** A persona the assistant proposes shows **Proposed —
  review it** (or **Proposed edit to …** when it changes one you have) and does
  nothing until you press **Approve**; **Discard** deletes it. You can also ask
  the assistant in any chat to create or improve a persona; it uses the
  `persona_studio` tool and the result lands here the same way.
- **New persona** opens the editor, optionally starting from a template
  (**Digital twin**, **Patrol analyst**, **Writing partner**). Set a display name
  (the short name is derived from it and cannot change later, because chats
  refer to it), one line on what it is for, **Digital twin** or **Specialist
  assistant**, **Suggest actions** or **Act on my behalf**, the workspace
  persona it **Builds on** (its expertise and tool access carry over — your
  persona adds instructions, it cannot unlock tools the workspace persona does
  not have), the instructions, and up to 8 suggested patrols (a title, quick
  cadence presets or the schedule builder for hours, minutes, daily, weekdays,
  or specific days of the week, a time-zone selector, and what to do each time;
  at least 5 minutes apart, with an **Advanced cron** option when needed).
  You can keep up to 50 personas.
- **Start chat** opens a new chat on the persona. **Start patrol** on a
  suggested patrol opens a new chat on the persona, attaches the patrol to it,
  and runs the first check right away. A persona set to **Act on my behalf**
  turns that chat's **⚡ Auto-approve** on; one set to **Suggest actions** leaves
  approval cards in place. Either way, autonomy is an instruction to the
  assistant plus that one default — the chat's approval setting still decides
  which tool calls stop for you.
- **Edit** changes every chat that uses the persona from its next turn.
  **Delete** leaves those chats in place; they continue on the workspace default
  persona.
- **Workspace personas** lists your deployment's personas: **Start chat** on
  one, or **Customize** to start your own persona from it.

For Bento presentations, your deployment can supply a default branded starter.
Ask for a different theme or a blank deck whenever you prefer; the starter's
layouts and styling are editable. Download the `.bento.html` file and open it in
a browser to view and edit it offline.

## 4. Getting data in

Four ways data reaches a conversation. Most days you will use the first two.

| Route | How it works |
| --- | --- |
| **Attach a file** | Click **Attach files** or drop a file onto the conversation, then ask about it in the same message. Spreadsheets, CSVs, PDFs, documents, and images all work. Files that are too large are refused when you pick them, with the limit stated, rather than failing later. An attachment belongs to the conversation it was sent in, and a note beside your message records what was added. |
| **Connected sources** | Mailboxes, reporting APIs, and other systems your deployment has connected. The assistant reads from them when you ask, using only the connectors turned on for this conversation. Name the source in your ask; see [the next section](#5-connectors-in-a-conversation) for how connectors are switched on. |
| **Shared file library** | Reference files an admin has published once for every conversation to read: historical data, price lists, lookup tables. They live under **Settings → Shared files**, and the assistant sees them in every chat without anyone re-attaching them. The assistant can also save text files there that every chat on the server can read, such as a team log of optimizations: ask it to "log this to the shared optimization log", and a brand-new chat can later answer "what did we do on this deal last month?" from it. Those files are marked **written by chats**; files an admin uploaded stay read-only to chats, and an admin can turn chat writing off in **Settings → Admin → Features**. |
| **Files the assistant makes** | Anything it produces in a conversation — a spreadsheet, a chart, an HTML report, or an image generated when the optional **Image generation** (`image_generation`) connector is on (**Google Nano Banana 2.1**, `google/gemini-nano-banana-2.1` by default) — stays in that conversation's working space. Download it from the transcript, preview HTML in place, and ask about it in later turns by name. |

> **Inline handles, if enabled.** Some deployments allow typed handles in a
> message: `@url:` followed by a web address pulls that page into the turn, and
> `@file:` followed by a filename pulls in a file the assistant made earlier in
> this conversation. Both are off unless an operator turns them on, so if a
> handle does nothing on your deployment, the feature is off and attaching works
> the same way.

For connectors that advertise binary file inputs, Fleet can send a hash-checked
workspace file chunk directly through the connector. This avoids copying large
base64 strings into the conversation and works without sandbox HTTP access.
The whole file can be up to 64 MiB, sent in chunks of at most 8 MiB each; the
connector may set lower limits of its own. The connector's existing permissions and publication checks still apply; a
staged upload alone does not mean a page or document was published.

Files from a link — a report link in an email, a signed download URL a
connector hands back, a public CSV — the assistant saves into the
conversation's working space with its download tool. A file can be up to 512 MB
(a larger one is not saved at all), and a download may take up to 5 minutes by
default, 30 at most. When a link first shows a "your download will start" page
or a page whose only link is the file, the assistant follows it to the file, up
to three pages deep. If a link that names a spreadsheet or report returns a web
page instead, usually a sign-in page or an expired link, the assistant is told
so rather than treating the page as the file. Links to private or internal
addresses are refused. Downloading the same link again reuses the earlier file
when nothing changed, and a download is refused when it would leave the server
short of free disk space.

## 5. Connectors in a conversation

A connector is a live link to a system: a mailbox, a reporting API, a document
store. Turning one on has two layers in Chat, and a third in the Operations
Center.

| Layer | Where | What it decides |
| --- | --- | --- |
| Availability | **Settings → Connections** | Which connectors exist for you at all, whether each is on by default in new chats, and which account it uses when a connector has more than one. Set once, lasts. |
| Selection | The **Tools** menu (the wrench) in the composer, its library, or an **Enable MCP connector?** card in chat | Which of your available connectors are live in *this* conversation. Narrow it when a chat only needs one source; if a task needs a connected bundled optional MCP connector that is currently off in the conversation, the assistant can suggest turning it on for you to approve (or turn it on itself when **Auto-approve** is active). |
| Binding | The task form on the Background tasks page | Which connectors a scheduled task may use, fixed when the task is created. A later change to your preferences never rewrites an existing task. |

**Settings → Connections** shows four connections under **Included with your
workspace** at first, and the ones you added yourself under **Added by you**.
A connection you added from **Find more connections** carries the name and
description of its directory card here and in the **Tools** menu; one you
added by URL under a name of your own keeps that name. If the directory now
lists a different address for a connection (it is marked **Directory
changed**), the connection keeps its own name until you remove it and add it
again from the directory.
A connection that needs you to sign in has an amber outline and shows **Sign in**
(**Sign in again** while it is connected, to refresh the sign-in). **Show all**
opens a popup with every workspace connector and the same availability,
new-chat, and default-account controls. A link to a connector outside the
preview opens that popup automatically. **Credential accounts** lists the named
accounts your connectors can use (one per client seat, for example), four at
first; **Show all** expands the list below it and **Show less** folds it back
up. The list is read-only: your administrator adds and removes these accounts
on the fleet server (`fleet mcp account set`), and their secrets are never
shown in the browser.

**Find more connections** starts with eight curated **Popular** picks:
Microsoft 365 (Outlook), GitHub, Slack, Gmail, Google Drive, Notion, Linear, and
Salesforce. A workspace with a different catalog may show different picks.
Below the search field, category pills (**Popular**, **All**, and category
filters with counts) and the **Explore by category** cards let you browse
connectors by category or view the full directory grouped by category with
**Show more** / **Show all** pagination. Search by name or keyword to find
connectors across the directory (or within the selected category); clearing the
search restores the currently selected view (**Popular**, **All**, or the
active category), and clicking **Back to Popular** returns to the popular picks. If the
connector you want is missing, use **Contact Elcano** below the search to
request it.

The Tools menu opens with its search field ready: type to filter by a
connector's name, its description, its category, or the name of one of its
tools (a match inside a connector shows under it, as in `↳ ix_clone_deal`).
Connectors that are on sit first under **On**, and the rest under their
category headings, such as **SSPs** or **Deal Management**, with **Misc
Tools** last. The **On** list keeps its order while the menu is open, so a
row does not jump as you switch it. `↑` and `↓` move through the list,
`Space` or `Enter` switches the highlighted connector, and `Esc` clears the
search, then closes the menu. A connector that needs setup, or is
unavailable right now, is hidden until a search finds it; it is shown dimmed,
with no switch, and selecting it opens it in the library. **All on** and
**All off** switch every connector the menu is showing, so with a search
they cover only the matches ("All 3 on"); the count beside them says how
many are on. The expand icon beside the search, or **Open library** at the
foot of the menu, opens the **Tools & connections** library: every
connector you can use in Chat with a category and status filter on the left, the list in the
middle, and the selected connector on the right with its switch, its status,
**Show N tools** for the names of its tools, and **Manage in Settings** for
whether it starts on in new chats. On a phone or a narrow window the filters
scroll sideways as chips, and tapping a connector opens its details on their
own screen; the back button at the top (or `Esc`) returns to the list.
A deployment can reserve some connector tools for scheduled tasks, typically a
raw write that Chat reaches through a reviewed flow instead. Chat leaves those
tools out of the Tools menu, its search, the library's **Show N tools** list
and every tool count, and does not list a **Bundled** connector whose every
tool is reserved. A third-party connector you signed into yourself is listed
either way, because its tools are only known once a conversation opens it. The
assistant cannot call a reserved tool in a conversation, whatever you turn on; a scheduled task still can, so the task form on the Background tasks page may show
tools and connectors that Chat does not. When a connector has more than one
account, an **Account** menu on its row and in the library picks the one this
conversation uses; the default reads as your deployment's name, or as the
account you chose in **Settings → Connections**. A connector that **Needs
setup** has credentials your administrator adds on the server; an
administrator's **Set up in Settings** link opens its card there. Fleet mounts at most eight third-party connections in one
turn — the ones you signed into yourself and any shared with you together
(Bundled ones do not count); turn on more and the turn tells the assistant
which were left out — the first ten by name and a count of the rest — so it
can say so when you ask for one. Keep a conversation to the connectors it
needs. Some connectors are marked **Always
on**: they are part of the deployment and cannot be switched off per
conversation, and the two buttons leave them alone. Each connector is also badged as
**Bundled**, shipped and operated with the deployment, or as a third-party
service you have signed into yourself, so you always know whose system you are
reaching. When a third-party connector needs a scheme in front of its API
key, the key form says so (for example `Authorization: Token token=<key>`);
paste only the key. A connector served from more than one host, such as a
vendor's EU region, shows an **Endpoint** choice on its card and on a
connection's **Add another account** form; pick yours before adding. A **Beta** badge means it works but still has rough edges. A
**Directory changed** badge on a third-party connection in **Settings →
Connections** means the directory entry of that name now lists a different
address, sign-in method or key placement from the connection; the row says
what differs. A connection keeps what it was added with, so remove it and add
it again from the directory to take the change — an existing sign-in cannot be
carried over. A connection you set up by hand under a directory name shows the
badge too, and can be kept as it is.

Most directory entries add with one click and then ask you to sign in at the
vendor. Some vendors (such as Slack, GitHub, or Google Workspace) do not let
 the platform register itself automatically. When your administrator has already
configured the workspace's OAuth app on the server, those entries are one-click
too: click **Add** and sign in with your own account. When no shared app is
configured on the server yet, the entry opens a short form instead: it shows
the **Authorization callback URL** to give the vendor, then asks for an
**OAuth client ID** and, where the vendor needs one, an **OAuth client secret**
(the form says when the secret is required). The client comes from an app
registered at the vendor for that callback URL — follow the setup note on the
card (for example, on Slack at `api.slack.com/apps`: create an internal
workspace app, add the **Authorization callback URL** under **OAuth &
Permissions → Redirect URLs** along with the **User Token Scopes** you want,
and turn on **Model Context Protocol** under **Features → Agents & AI Apps**).
An administrator can also set that app's client ID and secret once on the
server so everyone else in the workspace connects with one click. After the
form, you sign in at the vendor as usual.

When the service you need is not in the directory — or only has an HTTP API or
a web portal — click **Build a connection in chat** in **Settings →
Connections** (or type `/connector-builder` in Chat) to open a guided
Connector Studio chat. The assistant checks for an official MCP server first,
falls back to a custom HTTP API connector (badged **Custom API**) or a headless
Chromium automation connector (badged **Browser automation**, with an optional
live Co-Pilot browser view when you need to complete SSO, 2FA, or a CAPTCHA
yourself), collects any API key or password through a masked password field so
the secret stays on the host, and saves the connection for you alone or for the
whole workspace. A live browser view ends when you click **End Session**, when
the assistant closes it, after 10 minutes without activity, when Fleet restarts,
or when Fleet needs its place for a newer session; its link then opens a page
saying the session is no longer available, and the assistant can open a new one.

When the live browser view can't do something you ask, a short note at its top
says so, and for an address it won't open (one that isn't a web page, say), why.

Masked credentials submitted on a chat form stay in that conversation's
temporary vault as a `$secret:` reference. If a connector or browser call ever
carries a `[REDACTED]` placeholder, or passes a `$secret:` reference into a
scheduled task or saved custom connector (which use the connection's own stored
credentials instead), Fleet refuses the call rather than sending the
placeholder or reference text — submit the credential again on a fresh form or
update the saved connection in **Settings → Connections**.

Every connector call shows up in the execution trail as a chip. If a figure looks
wrong, open the chip and read what the connector actually returned before
questioning the arithmetic.

> **Credentials never leave the platform.** The assistant uses a connection; it
> never sees the credential behind it. Keys and tokens are held on the host, are
> brokered into a call only at the moment it is made, and never enter the
> sandbox, the conversation, or the model's context.

## 6. Cards that ask for a decision

When the assistant wants to do something with consequences outside the
conversation, it stops and stages a card. Nothing happens until you answer, and
if nobody answers, the answer is no.

That is how cards behave by default. A deployment can also put an individual
tool into **notify** mode, where the call runs first and its card is the record
of what was done rather than a gate before it — so read the card's buttons, not
just its presence: a card offering you a decision is holding the action, and one
that only offers to be dismissed is telling you about it. The rules below are
about the first kind.

| Card | When it appears | Your choices |
| --- | --- | --- |
| Send this email? | The assistant has drafted a message and wants to send it. The card shows the sender, the subject, and the To, Cc, and Bcc recipients each on their own line, lists every file that goes out with it by name, inline images included (visible even with the body collapsed), and shows the body (for HTML emails that exceed the preview height — or on a phone, where the collapsed preview lets vertical scrolling pass through — press **Expand full email** / **Collapse email preview** to view or interact with the full message). | **Send** · **Cancel** |
| Email preview | The assistant wants to show you a draft without sending it (with the same **Light** / **Dark** inbox toggle, **Mobile** / **Desktop** size toggle on wide screens, **Show source**, and **Expand full email** / **Collapse email preview** controls). | **Dismiss** — display-only by design |
| Run this shell command? | A command the platform treats as risky. | **Approve & run** · **Cancel** |
| Run "Connector studio"? · Run "Playwright browser"? | The assistant wants its built-in browser to type, click, press a key or choose an option on a page, usually while building a connection. The card lists each step in words, names where a password comes from (the form field you filled in) instead of showing it, and when the steps run in a live browser session shows the site the session started on and the site its page is on now, with a note when they differ. The call's own arguments, with the same values hidden, are under **Details**. | **Approve & run** · **Cancel** |
| Schedule a task | The assistant proposes a scheduled task, usually because you chose **Make a background task…**. See [From chat to a task](#10-from-chat-to-a-task). | **Approve & schedule** · **Edit** · **Cancel** |
| Change a task | The assistant wants to stop or update an existing scheduled task. A prompt change shows each edit as the text it replaces and the text it becomes, or text added at the end; the rest of the prompt is kept. A full rewrite shows the new prompt (a very long one is shortened on the card, and says so) under a warning that it replaces everything. (The assistant can read a task's current prompt without a card.) | **Approve & stop** or **Approve & update** · **Cancel** |
| Enable MCP connector? | The assistant needs one or more connected bundled optional MCP connectors that are not yet turned on in this conversation. Approving turns them on for this conversation and continues the reply with their tools mounted (unless the conversation's hourly auto-resume cap or input queue limit has been reached, in which case a notice is posted and the newly enabled tools take effect on your next message); when **Auto-approve** is active in the conversation, the assistant enables connected bundled optional connectors without stopping on a card and continues automatically when within those limits. | **Approve & continue** · **Decline** |
| Run an action | Any other connector action that changes something, such as updating a record in an external system. The card names the action, the system it runs against, and the arguments it was called with. A deployment can make it read in plain words instead: **APPROVAL NEEDED** with the countdown, a title, the record with its id and a link (**Open in …**), each change as before → after, and warnings such as "Deal is Active" as badges, with the raw arguments under **Details**. Long lists show the first 10 with **Show N more**, and a search box when there are more than 25. | **Approve & run** (**Approve** on a plain-words card) · **Cancel** |

Three rules for every card. **Read what is about to happen**, not just the
button: the card exists to tell you. **Cancel is free**: the assistant continues
without doing the thing, and you can ask it to try a different way. **Cards
expire**: a card left unanswered times out to no, and the transcript records that
it did. By default an action card waits a day (24 hours), and a conversation or
workspace can extend that up to 7 days.

After you approve, the card shows what actually happened: it ran, it failed, it
is still running, or — for an older card whose run finished before outcomes were
stored — that the outcome was not recorded. A plain-words card says it in one
line (**Applied**, **Not applied**, **Declined**, **Timed out — not applied**),
with the action's own result underneath and the records under **Details**.
A deployment can also have chosen actions report their own outcome. The
finished card then says what happened in one line ("Created 1 Index Exchange
deal · verified"), with a row per record: its name, its ids, a status
(**Done**, **Not applied**, **Check didn't match**, **Needs a check**,
**Outcome unclear**, **Already done**, **Not run**), and a link. The raw
result moves under **Details**. **Needs a check** means the record was
applied, but a later step for it failed (for example the deal exists, but its
exclusion list was not added); its note says what, and it is not offered for
retry, because the record already exists. When some records failed, **Retry failed (N)** asks the assistant
to try only those again, and the retry comes back to you as a new card to
approve: nothing runs from the click. A record that may already exist is never
offered for retry. An action that ran without asking can say what it did the
same way ("Campaign ID ELC07375 allocated"). A still-running card is not a new
decision: Cancel, Edit, and the expiry countdown are withheld, and **Check
result** fetches the outcome without running the action twice. In terminal chat
the same flags apply: `/approve <id>` on a running card retrieves the result;
an unknown historical outcome is an error, not success.

By default an approved connector action (a **Run an action** card, or an email
send) has a minute to finish. A deployment can give a slower connector's actions
longer (creating or updating a record in an external system, for example), and
such an action that is still running after about 50 seconds shows as still
running straight away rather than holding the page: use **Check result** a
little later to see how it ended. Shell commands and task cards keep their own
limits.

A deployment can also have chosen actions **report their progress** while
they run (creating many records in an external system, for example). Their
running card then shows a bar and a line such as **12 of 24 · creating deal
12**, refreshed every couple of seconds while the tab is open and still there
after a reload, and it switches to the outcome by itself when the action ends,
without **Check result**. If you have turned on browser notifications
(**Settings → General → Browser notifications**), you also get one when
such an action finishes (**✓ Done** or **✗ Not applied**, named by the card's
title), and one when the assistant could not carry on by itself after a card
was approved, declined or timed out; clicking it opens the conversation, in
your Chat tab if you have one open (it reloads, so an unsent message there is
lost) or in a new one. Safari on
a Mac does not open new tabs from notifications, so there it opens, after a
moment, in the Fleet tab you used last, which reloads too; with no Fleet tab
open, the click opens nothing.

Browser notifications go to the account that turned them on in that browser.
Signing out from the account menu turns them off there, so the next person to
use the browser does not get yours; closing the tab or letting your session
lapse does not, so sign out on a shared computer. The switch shows whether this
browser gets **your** notifications: if someone else turned them on in it, or
they are no longer on record for you, it reads off, and you can turn it on
again.

A deployment can also **group** chosen actions (booking the same plan in
several external systems, for example): when the assistant asks for two or
more of them in one reply, they arrive as one card titled **N actions to
approve**, one row per action, each with a checkbox, its own plain-words card
or arguments, and the account it runs as. **Approve all (k)** runs the k checked
actions and declines the unchecked ones in one decision; **Cancel all**
declines them all; **One at a time** shows the usual separate cards instead.
The countdown shows the earliest deadline among them; if it expires, the banner
offers **Review one at a time** so you can switch to the individual cards,
retry any expired actions, and decide any that are still pending. Once decided, each action
shows its own outcome card (running, applied, declined), exactly as if you had
decided it on its own card.

A deployment can also let you **stop** chosen actions while they run (a plan
working through many deals, for example). Their running card then has a
**Stop** button: it asks the connector to stop after the record it is on,
and the card reads **Stop requested** until the action ends. Whether it
stopped there is up to the connector, so the finished card's result says what
ran; a finished card with no outcome summary of its own is titled **Stop
requested**, never **Applied**. Where the connector does summarise its
outcome, the finished card says how far it
got (for example "Stopped after 12 of 24") and offers **Run the remaining N**,
which asks the assistant to run the records it never reached; that comes back
to you as a new card to approve. If the connector does not support stopping,
the action runs on to its end and the card says so in its result.

A deployment can also let you **leave records out** of one action that covers
several (a plan that updates many deals, for example). Its plain-words card
then has a checkbox on each record, all ticked, and a count such as **3 of 3
selected**. Untick a record to leave it out: it greys out and reads **Left
out**, and the button becomes **Approve 2 of 3**, which runs the action for the
ticked records only, in one click. Nothing else about the action changes, and
it can never add a record you did not see. Unticking every record turns the
button into **Decline**. On a grouped card each action's records have their own
checkboxes, and an action with none ticked is declined. After you approve, the
card says **Approved 2 of 3 · 1 left out** and marks the left-out record, and
the assistant is told which ones you left out. In terminal chat, `/approve`
approves the whole action.

A deployment can also have the assistant **carry on by itself** once you decide
a card for chosen actions (updating a record in an external system, for
example), so it can check the result and continue without you typing "done";
and if an action you approved fails with an error (and you did not press
**Stop**), the assistant automatically starts one recovery turn to diagnose the
error and fix or retry it gracefully.
A couple of seconds after you approve, decline, or the card times out — or,
for an action that was still running, once its result is in — a new turn
starts on its own. It opens with a muted **Continued automatically after an
approval — not typed by you** line (**Show** reveals the exact note the
assistant was given). After you approve or decline in the open chat it streams
like any reply; a turn started because a card timed out appears when you come
back to the tab or reload. If several such cards are
waiting, it waits until you have decided them all and then continues once. If
a turn is already running, it waits behind it as an **auto-continue** item in
the queue under the composer, which you can remove. It never approves anything
on your behalf: anything it then wants to do that needs approval shows a new
card. By default a chat continues on its own at most 10 times an hour; past
that, or if fleet restarted before it could continue, a short note in the chat
says so and you send a message to carry on.

Some cards carry a checkbox that widens your decision to every later call of the
same kind in this conversation, turning the button into **Approve + allow all**.
Leave it unticked unless you know exactly what the rest of the conversation will
do. A deployment can turn the checkbox off for actions that need your decision
every time; their cards have no checkbox, and each call shows its own card. You
can also set the conversation's **Approval mode** under **More options** (`•••`)
in the composer toolbar (even while a reply is streaming): **Manual** asks on
every critical tool, **Auto-approve** shows a lightning-bolt badge on the
toolbar and runs session-approvable connector actions and shell commands without
stopping on a card, and **Autonomous** (after confirming the Autonomous risk
prompt) shows an amber **Autonomous** badge and runs live platform actions,
optional connector activations, self-addressed owner emails, and eligible
prefilled forms (after a 10-second countdown you can pause) without pausing
while recording durable receipt and result cards in the chat (external
messages/invites, scheduled-task and patrol schedule changes, destructive
storage or permission changes, bundle-blocked actions in
`agent_policy.critical_tool_autonomous_blocked`, and `git push` still ask, and
only turns sent by the conversation owner use Auto-approve or Autonomous). Cards are not for exploring what an action would change; ask the assistant to
describe the change first, then approve the card once you agree with it.

Which tools are gated and which merely report is set per deployment, not by the
assistant, and it is worth knowing which is which for anything that touches a
system outside the conversation. Your administrator can tell you.

### Two gentler cards

**Proposed memories.** When the assistant learns something worth keeping about
you or your work, it proposes a memory rather than saving one: **Save** or
**Don't save**. The card shows where it will go — **Save to: My memory** or
**Team learnings**; inside a project shared with your team, Team learnings is
preselected and you can flip it. See [Memory and projects](#7-memory-and-projects).

**Model suggestions.** When a question is heavier than the current model handles
well, or a reply clearly struggled, a card offers **Switch & retry**, **Just
switch**, or **Dismiss** (unless the reply is already waiting on an action
approval card). Declining costs nothing, and after you dismiss one you
won't see another for a few messages. If you switch and later move back to the
fast model, a suggestion can appear again.

### Interactive cards

When a visual answer works better than prose, the assistant can build a card on
the spot: a form that collects several details at once, a list of items you can
add to, duplicate and remove, options it fetched from a connected system, a
checklist of what passed and what needs fixing, a table to pick rows from, a
chart, or a small calculator whose totals update as you type. It decides when a
card helps; you can also simply ask for one ("give me a form for this").

- **Fill it in, then press its button.** Fields marked with a red asterisk are
  required; the card says how many need fixing and takes you to the first. A
  **Fix** link next to a checklist line jumps to the field it is about. Self-contained calculators, checklists, and what-if cards update their totals, charts, and progress bars live as you adjust inputs without needing to send a message, and may offer a copy button (**Copied**) to copy a formatted summary to your clipboard or a **Reset** button to restore default values. Once you change any input on an editable card, **Reset** also appears in the card's header (whenever no usable reset button is shown in the footer) to put the starting values back and clear any saved draft.
- **Your answers become your next message.** Pressing a button sends what you
  entered as an ordinary message, shown as a compact summary of the card's name,
  the button you pressed and your answers (**Show what was sent** reveals the
  exact text); if it could not be sent, the summary says **Not sent** and the
  card stays open to try again. The card itself never acts: the assistant reads your answers and
  carries on, and anything it then does that needs approval still shows its
  usual approval card.
- **Some buttons ask first.** A button that asks a question before sending
  shows **Yes** and **Back** in its place; **Back** (or `Escape`) returns
  without sending.
- **A sent card locks.** It keeps your answers on screen. **Edit and resend**
  unlocks it to change something and send again; **Cancel edit** puts back
  what you sent.
- **A card waits for its answer to arrive.** If the assistant is still
  replying, or the connection dropped as you pressed the button, the card
  shows **Sent · … — waiting to reach the assistant** and keeps its buttons and
  fields off so you do not send twice. **Unlock** frees them if you want to
  send again anyway. If your answer never arrives (the connection recovers and
  it is not there, or you remove it from the queue under the message box), the
  card unlocks by itself and says **Not sent. Try again.**
- **An optional single choice can be undone.** A list of round options shows
  **Clear choice** once you pick one, and a table you pick one row from shows
  **Clear selection**, if the question is optional. A table where you can pick
  several rows has a checkbox in its header to select or clear every row at once.
- **Updated cards replace older ones.** When the assistant checks your answers
  and comes back with a corrected version, the earlier card collapses to a
  one-line note (**Show** opens it), with any problems marked on the new card's
  fields.
- **Unsent answers are kept in this browser** while you scroll or reload, until
  you send them, for up to 7 days and for the 20 cards you edited most recently.
  Sending from a card leaves whatever you are typing in the message box alone.
- **Very large answers are not sent.** If your answers (usually a long pasted
  list) are too big for one message, the card says so and sends nothing.
  Shorten the list and press the button again.
- **Cards from before a compaction are read-only.** Once a conversation is
  compacted (see [Long conversations](#long-conversations)), a card from the
  summarized part can't be answered, because the assistant no longer has that
  card's details. Ask it to show the card again.

Interactive cards appear only in Chat in the web app. Scheduled tasks never show
them. Terminal chat names the card and points you to the web chat; editor
clients connected over ACP show only the raw tool call, so open the
conversation in the web app to use the card. A shared or read-only view shows
the answers you sent but not the card.

## 7. Memory and projects

Two ways to give the assistant context it keeps. Memory is yours and travels with
you into every conversation. A project is shared with your team and shapes every
conversation inside it.

### Memory

A memory is a short fact the assistant is given at the start of every one of your
conversations: a preference, a standing rule, a piece of context about your work.
Your memories are personal; nobody else's chats see them. A project adds a
second, shared kind, covered below.

Memories arrive four ways. You can type one into the manager. You can tell the
assistant to remember something in a conversation, and it will propose the memory
as a card for you to **Save** or **Don't save**. You can press **Save to memory** in the
footer of any reply to keep what the assistant just said, without composing a
"remember this" turn. And the platform may propose memories it noticed on its own
after a turn, marked **Proposed** in the manager until you accept them. Nothing
enters memory without your say-so except what you typed yourself.

Open **Memories** from **More options** (`•••`) in the composer or under **Settings → Memories**. Inside a project chat, the manager has two tabs, **My memory** and **Team learnings**. Each memory
shows where it came from (**Manual**, **Saved from chat**, or **Auto-extracted
from chat**) and offers these actions:

| Action | What it does |
| --- | --- |
| **Pin** | Always given to the assistant first, and protected from being replaced by a newer memory. Use it for the rules that must never lapse. |
| **Retire** | Keep it for the record, stop giving it to the assistant. Retiring does not claim the fact stopped being true; it stops it being cited. The right move for anything stale. |
| **Restore** | Bring a retired memory back into use. |
| **Move to team learnings** | Move a personal memory into a project's team learnings so every member's chats in that project get it. It moves rather than copies: the memory stops applying to your conversations outside the project, and it is not given to the assistant twice inside it. If a rule should follow you everywhere and also serve the team, keep the personal memory and add a team learning separately. If you are in several projects, you pick which one. |
| **Delete** | Remove it entirely. |

> **Good memories are rules, not data.** "Deal names may contain intentional
> typos; never correct them" is a memory. Today's spend figure is not. Memory is
> for how you want work done; the data belongs in the sources.

### Projects

A project is a workspace for a body of related work: an account, a campaign, a
family of reports. It carries **instructions** that every conversation inside it
follows, and **team learnings**, a shared memory that every member can read and
add to. Any conversation in the project, whether started there or moved in later,
has both in place, so a team working on the same thing does not re-explain itself
chat by chat. Conversations inside a project are exempt from the expiry rule.

Every conversation in a project is fed three layers of context, in this order:
the project's instructions, its team learnings, then your own memory.
Instructions are set by the owner alone; team learnings are written by any
member.

### Setting one up

**New project** (in the **+** menu at the top of the rail) asks for a name, an
optional set of instructions, and **Who can see it**: **Only you** (a private
project) or your team. Shared with your team, everyone on it sees the project's
instructions, its team learnings, and any chat shared with them — but each chat
stays **Only you** until its owner shares it. You need to be on a team for the
team option to be available. Teams are set up in **Settings → Team** and members
are added by an admin, or, when your deployment signs in through central Auth,
both are done there by an administrator. Creating a project lands you on its home. The same choices
are reachable later from **Project settings** (the gear on the project home, or
the project's menu in the rail), which only the owner can open.

The project's owner can hand it over with **Transfer…** to another member, so a
project outlives its creator's involvement; transfer is available once the
project is shared. You keep using the project as a member of its team; if you
have left that team, handing it over also ends your access, and your chats in
it become temporary again — the transfer panel says which applies. Deleting a project asks first and names what goes with it:
its team learnings, and how many people's chats leave it and become temporary
again, with **Export first** offered. Switching a shared project to **Only you**
shows, before you save, how many shared chats become Only you and how many
teammates' chats move back to their own unfiled chats. Nothing in either case
deletes a conversation; chats always stay with their owner.

### The project home

The rail lists every project you can see: pinned ones in **Pinned**, the rest
under **Projects** by when each project was last created or edited (renamed,
or its settings or sharing changed) — chatting in a project does not move it
up. **Projects** shows the first eight, with **All N projects** for the rest;
the rail scrolls as one list, so no section scrolls on its own.

Open a project from the rail to reach its home. Its title carries a pill saying
who can see it — **Only you** or **Shared with** your team; in a shared project
the pill explains what the team sees. Until you have shared a chat in the
project, a short **getting-started card** walks the next steps (share the
project, add instructions, share a chat), each with a button that does it. The
**×** hides it for now; in a personal project, **Keep personal** hides it for
good. Then, top to bottom:

| Area | What it holds |
| --- | --- |
| **Your chats** | Your conversations in this project, with how many are shared with the team and a search box scoped to it. Each row has a pill, **Only you** or your team's name (plus a **Live** badge when **Multiplayer** mode is on): switch it to share the chat (and its files) or to stop sharing it; **More sharing options…** opens the share dialog. In a shared project, **New chat** asks whether the new chat starts as **Only you** or shared with the team. |
| **Shared by your team** | Conversations your teammates have shared into this project, with the owner and date. Open a read-only shared chat to read or branch it, or open a chat marked **Live · Multiplayer** to join the live conversation directly; see [Sharing with your team](#sharing-with-your-team). A row you have already branched says **You branched this**, and still opens the owner's live chat. |
| **Instructions** | The standing instructions. Editable by the owner. |
| **Team learnings** | One row per learning, each with who wrote it and when. **New team learning** adds one. The row menu holds **Pin**, **Edit**, **Retire** or **Restore**, and **Delete**. The same list is the Team learnings tab of the memory manager, so you can tend it without leaving a conversation. |
| **Sources** | Files, grouped by chat. In a shared project, **From your team** comes first — the files teammates shared, each downloadable — then **Your chats**. Each of your groups shows how many files it lists and, for a chat shared with the team, how many of them are shared; the toggle on a file shares or unshares it, and the arrow downloads it. Files you uploaded and your personal skills never appear here; neither do hidden or internal files (names starting with a dot) unless the assistant presented one to you in a reply. Copies that came with a branch are labeled **Your copy**. Each section lists the 50 most recently active chats with files; past that, the panel says it is showing the newest files only. |

Permissions in one line: **members manage their own learnings; the owner manages
all of them.** Retire is the polite default when a learning stops being true. It
stops being given to the assistant, and the record of what was learned and by
whom survives.

> **What belongs in team learnings.** The same test as personal memory, applied
> to the team: rules and context, not data. "The client wants deal names shown
> exactly as they appear in the source report" is a learning. This week's numbers
> are not. A learning that everyone on the project keeps re-stating in chat is
> the one to write down.

Any existing conversation can be filed into a project with **Move to project**,
and taken out again with **Remove from project**, which confirms that the chat
becomes temporary again and offers **Pin it and remove** so nothing expires by
accident. If a project is more structure than you need, **labels** are the
lighter option: tag conversations from their menu, then choose **Filter by
label** in the menu beside the **Chats** heading.
Labels organize; they carry no instructions or learnings.

## 8. Keeping, shaping, and sharing conversations

Every conversation in the rail has a menu behind the `⋮` button that appears when
you hover over it (on a touch screen it is always shown). The menu is the same
on every page. From a page other than Chat, an item that needs one of Chat's
dialogs (sharing, downloading, a patrol, archiving, deleting and so on) takes
you to Chat and carries on there; **Pin**, **Rename** and **Labels** happen
in place. Here is everything in it, in order.

| Item | What it does |
| --- | --- |
| **Pin** | Keep it indefinitely and show it under **Pinned**. Pin anything you would mind losing. A chat in a project has no **Pin**: it is kept with its project. |
| **Move to the top** | Keep it among the top items of the rail, above your lists; it reads **Remove from the top** once it is there. See [Customizing your sidebar](#customizing-your-sidebar). |
| **Rename** | Conversations take a title from their first exchange. Rename the ones you keep so the rail reads as a list of work, not a list of first sentences. |
| **Move to project** | File it under a project. The conversation picks up that project's context from its next turn. |
| **Labels** | Add or remove labels. **Filter by label**, in the menu beside the **Chats** heading, then shows only the chats that carry the labels you tick. |
| **Set as start page** | Make this chat the page fleet opens to; it reads **Remove as start page** when it is. See [Customizing your sidebar](#customizing-your-sidebar). |
| **Add a patrol…** | Opens the chat's **Patrols & timers**, to put it on a schedule; it reads **Patrols…** once the chat has one. Not offered on a sealed chat. See [In-chat patrols and timers](#in-chat-patrols-and-timers). |
| **Make a background task…** | Ask the assistant to turn this conversation into a scheduled task. See [From chat to a task](#10-from-chat-to-a-task). |
| **Save as workflow…** | Turn the whole conversation into a reusable workflow template in the prompt library or a personal **Agent Skill (`/skill-name`)**. See [From chat to a task](#10-from-chat-to-a-task). |
| **Share…** | Opens the share dialog: sharing with your team first, then **Share outside your team** (a public link). See [Sharing with your team](#sharing-with-your-team). |
| **Download chat…** | Export the conversation in one of three formats: **Web page**, which opens in any browser, looks like the chat, and prints to PDF; **Text document**, for pasting into email, Word, or Docs; or **Raw data**, the complete record for developers. **Include the agent's work** adds the tool calls, results, and reasoning; it is off by default so a long chat downloads as the conversation, not its machinery. |
| **Transfer ownership…** | Hand the chat to someone else; see **Transferring a chat** under [Sharing with your team](#sharing-with-your-team). Not offered on a sealed chat. |
| **Archive** | Put it away without deleting it. Archived conversations never expire and can be unarchived from **Archived**, at the foot of the **Chats** list. Archiving pauses the chat's patrols and timers, so a chat nobody is reading stops running; they stay paused when you unarchive it, until you resume them from the Patrol bar. Archiving a chat shared with your team stops sharing it (it asks first, with the number of shared files); unarchived, it comes back as **Only you**. An archived chat can't be shared with your team; unarchive it first. |
| **Delete…** | Remove the conversation and everything it produced. There is a confirmation, and no undo. For a chat shared with your team, the confirmation says what the team loses; teammates who branched it keep their copies. |

To pin, label or delete several conversations at once, choose **Select
chats…** in the menu beside the **Chats** heading. Tick the chats you mean,
then use the bar at the foot of the rail: **Pin selected**, **Add label**,
**Delete selected**, and **Exit selection** to finish. If the selection
includes chats shared with your team, the delete confirmation says how many,
and that the team loses them and their shared files; teammates who branched
them keep their copies.

> **What expires, and when.** An unpinned conversation that is not in a project,
> not archived, not shared, and has no patrol or timer is temporary: it is
> deleted after a period without activity — 90 days by default. Sending a
> message in it starts the period again. For the last part of the period (14
> days by default) the chat is flagged: its row in the sidebar shows the days
> it has left, a line above the list counts the chats about to be deleted, and
> the chat itself carries a notice with a **Pin to keep** button. There is also
> a per-person cap on how many temporary conversations are kept at all (500 by
> default): past it the least recently active go first, and the sidebar tells
> you when you are close. Pinning, archiving, filing into a project, or
> sharing exempts a conversation from both. The files and images you send in
> a conversation are kept for as long as the conversation is; a file you
> attach but never send is removed after the same period. Nothing is sent by
> email, and a deleted conversation cannot be brought back.

### Ordering your chats

What you pin and the chats in a project start out newest first. To put them in
your own order:

- **Pinned:** press **Reorder** in the **Pinned** heading.
- **A project's chats:** open the project's `⋮` menu and choose **Reorder
  chats**.

Each chat then shows a handle and a pair of arrows. Drag a chat to where you
want it — with a mouse by any part of its row, on a phone or tablet by the
handle at its left — or press its up and down arrows, which also work from the
keyboard. Every move is saved as you make it, to your account, so the order is
the same on every device you sign in from. Press **Done**, above the list (or
`Esc`), to go back to the ordinary rows.

The **Pinned** list is one order for everything in it: a pinned project and a
teammate's chat you pinned are moved like your own chats, so a project can sit
between two chats. The full Customize panel has the same list, with arrows.

A chat you pin, or file in the project, for the first time appears at the top
of the list, above the ones you have ordered, until you move it; one you unpin
(or move out) and bring back returns to its old place. **Reorder** is offered for
up to 500 chats at a time, and not to a read-only (viewer) account, which
cannot save an order. In a project you order your own chats; each teammate
orders theirs.

The **Chats** list has a sort instead, in the menu beside its heading:
**Recent activity** (the default), **Newest first** (by when the chat was
started) or **Title A–Z**. The choice is remembered on this device. Temporary
chats expire when left alone, so they are sorted rather than hand-placed; to
give one a place of its own, pin it or move it into a project.

### On the transcript

**Edit message** on any of your own messages lets you change it and resend from
that point. On any reply, the footer offers **copy** and **branch**; under the
latest reply it also offers **regenerate** (ask again, optionally on a different
model) and **Retry with** the stronger model (ask again on it, and keep it for
the rest of the chat). **Branch** starts a new, independent conversation
that begins with everything up to that reply, so you can explore an alternative
without disturbing the original. Branch when you want to keep both paths; edit
when the earlier version was simply wrong. The footer's two save actions, **Save
as workflow** and **Save to memory**, are covered in [From chat to a task](#10-from-chat-to-a-task)
and [Memory and projects](#7-memory-and-projects).

### Sharing with your team

Sharing a conversation with your team is how a good piece of work travels. A chat
is shared with a team only when it sits in a project shared with that team **and**
its owner shares it; sharing the project alone never shares its chats.

**The share dialog** (**Share this chat** inside **More options** `•••` in the composer, the visibility badge in the header, or **Share…** in a
chat's menu) always leads with your team and offers one button that does the
whole job for the situation you are in:

| Situation | The button |
| --- | --- |
| The chat is in no project | **Move and share** into a project already shared with your team, or **Create shared project** if there is none yet — the chat moves in and is shared. |
| The chat is in a personal project | **Share project first**, which takes you to the project home with the confirmation open; afterwards the getting-started card offers to share the chat in one click. |
| The chat is in a project shared with your team | **Share with your team**. **Includes N files · Choose…** lets you untick files before sharing. |
| The chat is shared | **Copy link for your team**, plus **Stop sharing** (which asks first and, when it can count them, says how many files stop being shared). |
| You are not on a team | Nothing to share with yet; the dialog says how to get onto one. |

**What it shares.** Your messages, the assistant's replies, and the **files the
assistant presented in its replies** (the file links in the chat) — including
files it presents later, because a shared chat is live. Files you untick stay
unshared, even if you stop sharing and share again or archive and unarchive the
chat. Files you uploaded are never shared, and files the assistant wrote without
presenting them are not shared either. Tool calls, tool results, and reasoning
are left out. In your own shared chat, each file link shows **Shared** or **Not
shared**; click it to manage files in the project's **Sources**.

It shares **read-only**: a conversation has exactly one owner, and nothing a
teammate does changes yours. Teammates see new messages as you keep working.

**The link for your team** opens only for signed-in members of your team, on the
read-only view — or, for a chat in **Multiplayer mode**, straight into the live
chat for everyone who can take part in it, so they join it instead of
branching it. Anyone else who opens it sees that the chat is shared with your
team and nothing about it; a signed-out teammate is sent to sign in and then
straight to the chat. Once a chat stops being shared, its link says so.

**When sharing ends.** A team-shared conversation always has a home: the project
it sits in. Moving it into another project shared with the same team keeps it
shared. Moving it anywhere else, removing it from the project, archiving or
deleting it ends the share — each asks first and says how many shared files go
with it. Making the project personal, deleting it, or leaving the team ends the
share too. Moving a chat into a shared project never shares it by itself; the
confirmation offers **Share with** your team.

**Share outside your team** is the separate public link: anyone with the URL can
read the transcript, never the files.

**Reading a teammate's chat.** Open it from **Shared by your team** on the
project home, or from a link they sent. You get the transcript with the owner's
name; shared files are live downloads, and files the owner kept back show as
locked names. Where the composer would be is one action: **Branch to continue in
your own chat**. That creates a private conversation of your own, filed into the
same project, starting with everything in the transcript, and **the shared files
come with it as your own copies** — the assistant in your branch can use them.
Your copies never change afterwards, even if the original is unshared or deleted.
When you open a chat you have already branched, a banner links to your branch and
says whether the owner has added messages since; **Branch again** starts a fresh
branch from the current version.

**Multiplayer mode.** For teams that want to jump in and work inside the same
live conversation together, the share dialog also includes a **Multiplayer mode**
section (**Turn on** / **Turn off**). New chats start as **Only you** by default,
unless an administrator has enabled **Multiplayer team chats by default** in
**Settings → Admin → Features** (`FLEET_TEAM_CHATS_SHARED_BY_DEFAULT`), in which
case new unfiled or team-project chats (outside Lockdown mode) created by team
members start in **Multiplayer mode** automatically (chats created inside a
personal project stay private until Multiplayer mode is turned on in the Share
dialog). When Multiplayer mode is on:

- Teammates (and anyone else added to the chat) see it in the rail, among
  their **Chats** by default, with the green dot of a Multiplayer chat, can
  open it directly in the chat view, and can send messages into the shared
  thread. It opens the same live chat however they reach it: from the rail,
  the project home, a search result, the link for your team, or the link
  back to it on a branch they made of it. Someone you removed, or who is not
  allowed to take part, gets the read-only view instead while the chat is
  shared with their team.
- Every message shows the sender's email above their bubble, and each turn runs
  under **the sender's own account** — using the sender's email, personal
  memories, skills, per-user connector accounts, and approval cards. Because a turn's tool calls, reasoning, and token/cost summary belong to the person whose credentials ran it, turning on **Show details** shows **Turn details hidden for other users' turns** on turns started by someone else. That keeps your screen clear; it is not a privacy wall. The assistant works from the whole chat, including what it fetched on a teammate's turn, and can repeat it, so turn Multiplayer mode on only with people who may see what the chat's turns bring in.
- By default, everyone on the owner's team (or workspace) can participate. In the
  share dialog under **People who can participate**, the owner can remove
  specific people with `×`, restore removed people with `+`, or invite someone
  with **Add person by email**.

**Transferring a chat.** To hand a chat to someone else, with its dashboard,
files and patrols, open the chat's `⋯` menu in the sidebar and choose
**Transfer ownership…**, then enter the new owner's email. They need an active
account here and can be on another team. The chat becomes theirs as soon as
you confirm; they are not asked to accept. Its patrols and timers move with it
and are paused, because a patrol runs with its owner's connectors: the new
owner presses **Resume** in the Patrol bar to start them again. The chat
becomes private to the new owner (team sharing, Multiplayer mode and any
public link are turned off, and approval mode goes back to **Manual**), and
you lose access to it unless they share it with you. An archived or Lockdown
chat can't be transferred, and a chat with a turn running has to finish or be
stopped first. An admin can also transfer a teammate's shared chat from
its `⋮` menu.

### While a turn is running

**Stop** halts the current turn; the partial reply stays in the transcript,
marked **Turn stopped.** with **Retry** on the latest turn, and it still reads
that way after a reload. A turn the platform ends early is marked the same way:
one that reaches the per-turn time limit (4 hours by default) or cost or
token ceiling, or one a server restart cut off partway, which also carries a
note saying so.
Typing and sending while a turn is running does not interrupt it: your message
waits in a small queue under the composer, with **Send now** to move it to the
front and a control to remove it. Queued messages run in order once the current
turn finishes, each appearing on screen as its own turn, and the queue survives a
reload or a switch to another chat.

### Messages from an administrator

A workspace administrator can send a message into your chat, for example to
ask a patrol to publish a log it keeps. It appears in the transcript as
**Message from an administrator** with their email, never as your words, and
a short note in the chat says what was changed and why. The reply runs with
your connections, and always asks you before anything that needs approval,
even if the chat is set to approve automatically. An administrator can also
change one of your patrols' instructions or its schedule (how often and at
what time it runs), or pause or resume it; each change leaves the same kind
of note in the chat. An administrator can also archive or delete a chat; a
deleted chat stays in a recycle bin for 30 days, so ask them to restore it if
it was a mistake.

An administrator can also **pin a note** to your chat. It sits above the
conversation, titled **Note from an administrator** with their email, until
they remove it. Press **Got it** to tell them you have read it; the note then
folds to one line that opens again when you click it. In a Multiplayer chat
everyone in the chat sees the note, and the first **Got it** counts for the
chat. The note is not part of the conversation: the assistant never reads it,
so typing a reply in the message box will not reach the administrator.

To help you, an administrator can read your chat. They cannot type as you or
answer your approval cards; those stay yours.

### Long conversations

As a conversation grows, **Summarize earlier messages** in **More options** (`•••`)
shows how full the chat is, as a percentage of the model's context window. With **Show
details** on, a totals line above the composer gives the same figure as a
percentage alongside the conversation's cumulative cost. When a conversation gets
full, a banner says so and the assistant starts to slow down and cost more per
turn. **Summarize earlier messages**, in **More options** or on the banner, replaces the earlier
turns with a short summary so the conversation fits comfortably again; the
transcript keeps its history, only the assistant's working view is shortened.
If the conversation changes while the summary is being written (a reply lands
from another tab, say, or a reply is retried), the summary is not saved and the
chat says *"The conversation changed while it was being summarized, so the
summary was not saved. Summarize again."* Summarize again to cover the
conversation as it is now.
You do not have to watch for it: by default, once a request reaches about 90% of
the model's limit, the platform summarizes the oldest part of that working view
by itself — between turns, or in the middle of a long turn that keeps pulling in
data — and a note in the reply says so. If a model still rejects a request as
too long, the platform summarizes and retries on its own instead of failing the
turn. When a topic changes entirely, a new conversation is cheaper and clearer
than compacting an old one.

## 9. The prompt library

The library is where a finished prompt lives. Chats are where prompts get built;
tasks are where they run; the library is the one place both of them look.

There is one library, visible from three places: **Prompt library** inside **More
options** (`•••`) in the composer, **Settings → Prompts**, and the prompt field in the task form on the Background tasks page
all open the same list, so a prompt saved from a conversation is, word for
word, the prompt a task can run. It holds two kinds of entry:

| Kind | What it is |
| --- | --- |
| Deployment prompts | Shipped with your deployment and kept up to date from outside the app. Read-only, marked **Git** in the list. Use them as they are, or insert one and adapt it into a prompt of your own. |
| Workspace prompts | Created by your team. Each is either **Private**, visible only to its author, or **Workspace**, visible to everyone on the deployment. Only the author or an admin can edit or delete one. When an administrator deletes the author's account, their **Private** prompts are deleted with it; their **Workspace** prompts stay, marked **Shared by a deleted account**, and only an admin can edit or delete them. |

### Using a prompt

Open the library, search or scroll, and select an entry to read it in full. **Use
prompt** inserts it into your draft; send it as it is, or add to it first. On the
Background tasks page the same action fills the task's instructions and seeds the
task's title from the prompt's name.

Some deployment prompts are **forms**, marked **Form** in the list. Selecting one
shows its questions instead of its text. Fill in the fields marked `*`; some
forms keep extra settings under **More options**. **Use prompt** stays
unavailable until every required field has an answer, and the **Prompt preview**
under the form shows exactly what it will insert. An optional text, number or
date field you leave empty is left out of the prompt, so there are no
placeholders to hunt down afterwards. The finished prompt lands in
your draft like any other, ready to read, change or attach files to before you
send it. **Insert raw prompt** inserts the template with its `{placeholders}`
instead, if you would rather fill it in by hand.

### Creating and editing

**New prompt** opens a form with a name, an optional description ("helps
teammates find it"), and the prompt text. If you already have text in the
composer, it is carried into the form. **Share with this workspace** decides
Private or Workspace; leave it unticked until the prompt has been reviewed and is
the team's agreed version. Your own entries offer **Edit**, **Duplicate**, and
**Delete** when selected; deployment and teammate-shared entries offer **Clone &
edit** so you can customize a copy of your own.

One convention worth adopting from the start. **One team version per report**:
once a prompt is shared, the private drafts that led to it should go, and the
task that runs it should be created by whoever will maintain it, since a task's
row is visible only to its creator and to admins.

### Backing up

**Back up JSON** downloads the library as you see it, deployment prompts
included, as a plain file. Keep a copy in the team's shared drive now and then;
it is the whole of your prompt work in one file.

> **Why the library is the contract.** A task runs whatever it was given when it
> was created. If the report needs a fix, fix the library prompt, then re-select
> it on the task so the task picks up the new version. Editing instructions
> inside a task instead creates a fork nobody else can see, and the library copy
> quietly stops being true. The task's other settings — its schedule, its
> recipients, the connectors it may use — are fields of their own on the task
> form, and inserting a prompt there does not disturb them.

### Email templates

If your deployment has standard emails, **More options** (`•••`) also lists
**Email templates**. A template is the fixed shape of an email the team sends
again and again, such as a weekly client update: the same sections, in the same
order, at the same length, whoever asks for it and in whichever chat. The entry
is not shown on a deployment that has none.

Select a template to see two things side by side: an **example email** built
from sample data, and the template's **sections in order**, each with what
belongs in it and its limits (for example *Up to 3 bullets*). Below them are the
rules for the whole email and the phrases it never uses. Some templates come
with your deployment; others are ones your team added, marked **Team** with
the name of whoever added them.

Choose **Use this template** and fill in the short form. Besides the
template's own questions it can ask who the email goes to and how it looks:

- **Send to** lists the addresses you mail most under **Frequently used**;
  select one to add it, or type any other address and choose **Add**. An
  address you type is offered again next time. Leave it empty to get a review
  copy yourself.
- **Style** shows each look with its colors, and the example email beside the
  form takes on the look as you pick it. **Custom colors** lets you pick a
  client's header and accent colors instead, and **Recently used** brings back
  the looks you used last, so you set a client's colors once.

Then choose **Use prompt**. Like a library prompt, this
only puts a request in your draft; nothing is written or sent until you send
the message. The assistant then writes the email to that template and puts it
on an approval card that shows the email and names the recipients. It is sent
only when you choose **Send** on that card; reject it, or ask for changes, and
nothing goes out. A draft that breaks the
template, by running long or adding a section, is not built: the assistant is
told what to fix and tries again, so what you get back fits the example.

**Adding a template.** When an email came out the way you want it every time,
ask the assistant to keep it: *"Save this email as a template called Monthly
QBR recap."* Or choose **New template** in the library, which writes that
request into your draft for you to finish. The assistant builds the template
from the email in the chat (its sections, their order and their length), checks
it, and asks you to approve it on a card. Once approved it is in the library
for your whole team, with that email as its example. The example is visible to
everyone, so ask for invented names and numbers if the real ones should not be
shown. To change a team template, save a new version under the same name; to
remove one, ask the assistant by name. Templates that came with your
deployment cannot be changed from chat.

## 10. From chat to a task

You have a conversation worth keeping. Two steps turn it into something that runs
on its own: get a prompt out of the chat, then get the prompt into a task. Each
step has more than one route, and the right one depends on what you want to keep:
the exact report, or the method behind it.

### Getting the prompt out of the chat

**Ask the assistant to write it.** The route for a report you want reproduced
exactly, every time. The assistant knows what it just built, and you can push
back on the draft in the same conversation before anything is saved. Ask for
something like:

> Write me a prompt I can save to the prompt library that will recreate this
> exact report in a single run. Lock in this layout as the template: same
> sections in the same order, same tables, same formatting, with only the data
> changing. It needs to work in a brand new chat with no other context.

Read the draft, ask for changes, then paste it into **New prompt**.

**Save as workflow.** The route for a method you want to reuse on different
inputs. From the conversation's `⋮` menu or the footer of any reply, the platform
reads the whole conversation — including what the assistant did and where it went
wrong along the way — and writes it up as a workflow template: an objective, the
inputs as bracketed placeholders, the steps with the tool each one used, the
output, and notes. This run's names, dates, and targets are generalized; the
method stays specific. In the save dialog, choose **Destination**: save it to the
**Prompt Library** (private by default, or **Share with workspace**) or save it
directly as an **Agent Skill (`/skill-name`)** (personal by default, or published
to **Workspace skills** so teammates can click **Add to my skills**).

Save as workflow is a good fit for the afternoon you spent getting the assistant
to profile a new dataset, compute a baseline, and draft a note, because next
quarter you want that recipe aimed at a different client. It is the wrong fit for
a locked daily report, where you want one exact question answered the same way
every morning; use the first route for that.

**Write it yourself.** Fine for simple asks. The checklist below still applies.

Whichever route, check the draft before saving. For a report prompt: every source
named; identifiers spelled out; layout and section order locked; and the rule
that anything unavailable is flagged and the rest continues. For a workflow
template: the placeholders cover everything that changes between runs, and no
step says "analyze the data" where the conversation actually did something
specific. If the draft mentions recipients or a run time, it will still work, but
those details are best set on the task itself, where they can be changed without
touching the prompt. Then **test it**: open a new chat, insert the saved prompt
from the library, and send it. You should get the same report on current data. If
it differs, go back to the build conversation, tell the assistant what changed,
and update the saved prompt.

### Getting the prompt into a task

**From the library, on the Background tasks page.** The recommended route. On
the Background tasks page, **New task** (or **New background task** in the rail's **+**
menu, from any page) opens the task form; insert the prompt from the
library, then set the schedule, the model, and the recipients. The prompt has one
home and the task points at it. The Background tasks guide covers the form field
by field.

**Make a background task… from the chat.** The direct route. From the conversation's
menu, the assistant proposes a task from the conversation and stages a schedule
card. **Edit** opens the name, the schedule, and the full prompt; **Approve &
schedule** (or **Approve with changes**, if you edited) creates it; **Cancel**
creates nothing. The name you confirm becomes the task's title on the board.
The card also shows the model the task will run on: the workspace's task model
(the stronger model by default), not the model this chat was using.

The trade-off, stated plainly: the prompt is generated from the conversation
rather than taken from the library, so review it in **Edit** with the same
checklist. Consider saving that reviewed prompt to the library as well, so a
later fix has somewhere to live.

Either way, the task appears on your own board on the Background tasks page.
Teammates receive its emails, but unless they are admins they do not see its row,
so the team's version of a report should be created by whoever will maintain it.

> **One rule holds on every route.** A task keeps the prompt it was created with.
> A library edit only reaches a task when the prompt is re-selected on that task.
> After any change to a library prompt, open the tasks that use it, re-insert the
> prompt, and **Run now** once to confirm.

From here, the **Background tasks guide** takes over: run states, logs, what to
do when a run needs attention.

## 11. When a turn goes wrong

Most problems announce themselves in the transcript. Match what you see to the
row below.

| What you see | What to do |
| --- | --- |
| **Retrying** | A connection or a service hiccupped, and the turn is retrying by itself with a short countdown. Wait. It usually clears on its own. |
| Nothing at all for a while | A model that reasons before it writes can be silent for a minute or more, and that is not a fault. By default the platform gives a model about a minute and a quarter to start. If it still has not started, you will see **Retrying** while the platform tries again by itself, and your deployment may try a backup model after that — which usually just answers, with nothing more for you to do. If the last attempt also never starts, a card says the model did not start responding. Retry from the card, or pick a different model. |
| **Turn failed** | The turn did not complete. The reply or the banner says why when it can. On the latest turn, **Retry** resends your last message. If it fails the same way twice, change something: the model, the ask, or the attachment. |
| **Out of credits** | The deployment's API key has run out of credits or reached its spend limit. Top up the API key, or use **Contact licensing@elcanotek.com** on the card to add credits, then press **Retry**. |
| Reply seems to stop | On a phone that locked, or a laptop that slept, the screen can lose the connection while the assistant keeps working. Wait a few seconds after the connection is back: the page re-checks by itself and either resumes the live stream or drops in the finished reply. If the turn had already failed while you were away, the page says what went wrong and offers **Retry** — or **Pick a different model**, when that is what the turn needed. After a long outage those checks space themselves out, so clicking back into the tab prompts one straight away. Refresh only if it is still stuck after that. The work was never lost either way. |
| A notice at the top: **… was updated** | A newer version of the app has been released since this page loaded. Press **Reload** when it suits you, or dismiss the notice with **×**. It never reloads the page by itself. The same notice appears on every page of the app, and also when the app is added to an iPhone or iPad Home Screen, where there is no reload button of its own. |
| A page that says **Something went wrong** | The page hit an error. Press **Reload**, or **Go to the home page**. A page left open across an update usually reloads by itself once to pick up the new version. If the same page fails again right after, it waits for you instead. |
| Model unavailable | The banner offers **Pick a different model**. Choose one and resend; the conversation keeps its history. |
| File refused | An attachment over the size limit is refused when you pick it, with the limit shown. Split the file, or trim it to the columns and dates you need. |
| Connector unavailable | The picker shows **Unavailable** against a connector, or the trail shows a call that returned an error. Check **Settings → Connections** for a sign-in that has lapsed; otherwise bring it to your administrator along with a **Download chat** export in **Raw data** format — that one always carries the full record, so it offers no options to tick. |
| Connector or browser call refused (`[REDACTED]` or `$secret:`) | A call tried to reuse a masked placeholder from earlier in the chat, pass a chat-only credential reference into a scheduled run or saved custom connector, or run a saved custom connector whose stored definition recorded the placeholder. Ask the assistant for a new credential form and submit the secret again, or update/recreate the saved connection in **Settings → Connections** with real credentials. |
| Conversation is full | A banner reports the percentage. **Summarize earlier messages**, or start a new one if the topic has moved on. |
| The answer is wrong | Open the chips in the execution trail and read what actually came back from each source. Then ask: "show me the rows behind that number". Most wrong answers are a wrong source or a misread identifier, and both are visible in the trail. |

Anything you cannot resolve from the transcript goes to your administrator. The
fastest way to hand it over is **Download chat** from the conversation's `⋮`
menu, choosing **Raw data**: that format always carries the full record,
including what every tool returned, so nothing needs re-describing and there is
no option to forget. (**Include the agent's work** is offered on the two
readable formats, which leave it out by default; Raw data does not show the
checkbox because it never omits anything.)

## 12. Working conventions

Seven habits that keep a shared deployment legible and its reports trustworthy.

| Habit | Why |
| --- | --- |
| Name conversations deliberately | Rename anything you keep so it reads as a piece of work. Titles are what you search, share, and hand to a colleague when something needs a second look. |
| Pin what matters | Unpinned conversations expire. Pin, file into a project, or archive anything you would mind losing. |
| Pick the model for the job | Of the recommended models, the fast one for pulls, checks, and formatting; the stronger one for judgment and narrative. Switch mid-conversation when the ask changes. |
| Name sources and identifiers | Say which report, which sender, which deal. The trail can only show you what you asked for. |
| Read the card before you approve | A card is the platform telling you what is about to happen. Cancel is always free. |
| Give every prompt one home | Build in chat, keep in the library, run from a task. Fix in the library and re-select on the task. |
| Write learnings down in the project | A rule the team keeps repeating in chat belongs in the project's team learnings, where every conversation gets it without anyone re-typing it. |

## 13. Keyboard shortcuts

Everything here also has a mouse equivalent. Press `?` anywhere outside a text
field to see the same list in the app. `⌘` on a Mac is `Ctrl` on Windows and
Linux.

| Shortcut | Action |
| --- | --- |
| `⌘ K` | Open search |
| `⌘ ⇧ O` | Start a new conversation |
| `⇧ Esc` | Focus the composer |
| `Enter` / `Shift Enter` | Send / add a line (or the reverse, if **Send on Enter** is off) |
| `J` / `K` | Move the focus cursor down / up the conversation list |
| `Enter` | Open the focused conversation |
| `P` | Pin or unpin the focused conversation |
| `A` | Archive the focused conversation |
| `R` | Rename the focused conversation |
| `#` | Delete the focused conversation (opens the confirmation; never a silent delete) |
| `Y` | Copy the last reply |
| `E` | Edit your last message |
| `D` | Turn [dashboard view](#dashboard-view) on or off (`Esc` leaves it) |
| `?` | Show the shortcut help |
| `Esc` | Close search, the help overlay, or the sidebar |

Single-letter shortcuts never fire while you are typing in a text field, so they
cannot interrupt a message.

Three chords you might expect are deliberately left alone, so your browser keeps
them: `⌘F` stays find-in-page (the natural way to search the transcript in front
of you), `⌘N` stays new-window, and `⌘J` stays downloads. That is why new
conversation and focus-composer sit on the two chords above instead.
