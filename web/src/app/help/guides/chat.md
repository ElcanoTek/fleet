# Chat

Chat is the working surface of the platform: the place you ask questions of your
data, analyze it with the assistant, build reports, and review the results. This
guide covers the screen and its controls, how to get answers you can rely on,
what the assistant can reach and what it will ask you before acting, and how the
work you want repeated moves from a conversation into the prompt library and
onto a schedule.

## 1. What Chat is

### Terminal chat

On the server, `fleet chat --email you@example.com` opens terminal chat. New
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
change an existing thread: use the web Tools picker for that.

Routine server updates, restarts, and stops (`fleet update`, `fleet restart`, `fleet stop`) refuse by default while a chat turn, approved action, dataset run, or scheduled task is in flight, and quiesce new work admission while draining when `--wait` is used (`--wait-timeout` sets the limit, 10 minutes by default; `--force` bypasses the check). If the server restarts during an approved action, its outcome is shown as unknown;
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
`to_email` (not the summary label `to`). Spaces are allowed in globs, and a
matching deny takes precedence over approvals. `/approvals` shows the available
handler-only pattern arguments; this requires a server that supplies them.
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
**Operations Center Guide** covers the rest.

> conversation → saved prompt → scheduled task → your inbox
> *(this guide covers the first two)*

The two surfaces divide the work simply. Chat is **hands on**: you are in the
conversation, asking, reading, and redirecting as it goes, and that is where a
report gets shaped until it comes back right. The Operations Center is **hands
off**: the same conversation, once it is right, runs there on a schedule and
delivers itself, and you only look in when something needs you. The skills are
the same in both places; only your role changes.

Quick-start cards in a new chat can collect a few inputs before starting work.
Text fields keep spaces as you type; larger text boxes accept several lines.
The prompt preview preserves those line breaks so you can check what will be
sent before pressing the card's run button.

### In-chat patrols and timers

You can also run a recurring **patrol** or a one-shot **timer** directly inside
a chat conversation, without leaving the thread or promoting it to a separate
Operations Center job. Each time a patrol or timer fires, the assistant wakes up
in the same conversation with its full message history, workspace files, and
enabled connectors intact.

Start a patrol or timer in either of two ways:

- **Ask in chat:** tell the assistant what to patrol and how often (for example,
  *"patrol my deals every 2 hours and update the dashboard if pacing drops"* or
  *"check back in 30 minutes and verify the backfill"*). The assistant uses the
  `chat_patrol` tool (`create`, `list`, `update`, `pause`, `resume`, `cancel`) to
  attach or adjust the schedule on the current conversation.
- **Use the Patrol button in the composer:** click **Patrol** in the composer
  toolbar, choose a preset (**Every 1h**, **Every 2h**, **Every 4h**, **Every
  6h**, **In 30m (1x)**, or **Custom**), enter the patrol instructions, and
  press **Start patrol**.

When a conversation has an active or paused patrol or timer, the **Patrol bar**
appears at the top of the thread showing the schedule's status (**Patrol
active**, **Timer armed**, or **Paused**), cadence, run count, and countdown to
the next run, along with **Run now**, **Pause / Resume**, and **Stop** buttons.
Enabling **⚡ Auto-approve** (in the Patrol popover, Patrol bar, or composer)
lets session-approvable tool calls run hands-free while you are away; if a
patrol turn does stage an approval card, the card stays open for 24 hours and
automatically resumes the patrol turn as soon as you approve it. Conversations
with an active or paused patrol are exempt from retention sweeps until the
patrol is stopped.

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

**New chat** starts a fresh conversation. The search field, also reachable with
`⌘K` or `Ctrl K`, matches conversation titles, and can search **In messages**
too. Below that, conversations are grouped:

| Group | What it holds |
| --- | --- |
| **Projects** | Workspaces with standing instructions, shared learnings, and their own conversations. A project shared with your team carries a team glyph. See [Memory and projects](#7-memory-and-projects). |
| **Pinned** | Conversations you have chosen to keep. Pinned conversations never expire. |
| **Labels** | Your labels, each a filter. Click one to show only conversations carrying it; click several to narrow further. |
| **Temporary** | Everything else, newest first. Chats here are deleted after a period of inactivity — 14 days on a default deployment. Pin a chat or move it into a project to keep it. |
| **Archived** | Collapsed by default. Conversations you have put away but not deleted; open the group to bring one back. |

A chat row can carry two small badges, each labeled with its audience when you
hover: one means it is shared by link, the other that it is shared with your
team. Click a project's name to open its home (and expand it); the chevron only
folds it. Hovering a project shows its menu: **Open project**, **New chat**, and,
for the owner, **Share…** and **Project settings…**. See [Keeping, shaping, and sharing conversations](#8-keeping-shaping-and-sharing-conversations).

Your account menu at the foot of the rail switches between **Light**, **Dark**,
and **System** appearance and opens **Settings**.

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
defaults to **Contributor**; **Viewer** is read-only. **Fleet Admin** grants full
Chat and Ops Center permissions and highlights the included Contributor choices.
Selecting Fleet Admin again turns it off and safely defaults to Chat Contributor
with no Ops access. Accounts created and granted Fleet in central Auth are added
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

### The header

Above the transcript sit three small controls, left to right: **Keyboard
shortcuts** shows the same list as the [last section](#13-keyboard-shortcuts) of
this guide; **Memories** opens the memory manager; and **Show details** toggles
the assistant's reasoning, working notes, tool calls, and per-turn cost figures on
or off across the whole conversation. When your operator configures a support
contact in the deployment's bundle (`branding.support`), the top bar also shows
that support button across Chat, the Operations Center, Settings, and Guides;
pressing it opens a dialog with your operator's message, a **Copy email** button,
and a **Send email** (`mailto:`) action (omitted by default when not configured).
Every conversation also shows who can see it — **Only
you**, **Shared with** your team, or **Public link** — and a **Share** button.
Both open the share dialog.

### The transcript

Your own messages appear as you typed them. When the platform adds something to a
message that you did not type, such as an attached file or a note from the shared
file library, it appears as a collapsed note beside your message rather than
inside it, so what you wrote and what was added stay visibly separate.

Each reply from the assistant is laid out in a fixed order, top to bottom, so
your eye lands on the answer and then on anything that needs you:

| Part | What it is |
| --- | --- |
| Execution trail | A row of chips, one per tool the assistant used: a file it read, a connector it called, code it ran. Click a chip to see exactly what went in and what came back. This is how you check the work. A chip in the danger colour is a call that failed; the chip of an action that went to an approval card is not: it is marked **sent for approval**, and the card says whether it ran. Shown only when **Show details** is on. |
| Reasoning | The assistant's narrated thinking. Also shown only when **Show details** is on. |
| Working notes | While a reply is in progress, the short notes the assistant writes between steps ("Let me look up the deal first."). Shown only when **Show details** is on, and only until you reload the page: the saved conversation keeps the answer, not the notes. With details off, a status line takes their place: **Thinking**, or the step under way, such as **Running shell** or the plan item it is working on. |
| The answer | Text, tables, charts, and images. Code and data come in copyable blocks. When the assistant produces an HTML report, it renders as an **HTML preview** with a **Show source** toggle. |
| Interactive cards | A form, picker, checklist, comparison, chart or small calculator the assistant built for this answer. Fill it in and press its button; your answers go back as your next message. See [Interactive cards](#interactive-cards). |
| Status | Banners for anything unusual about the turn: retrying, cancelled, failed. See [When a turn goes wrong](#11-when-a-turn-goes-wrong). |
| Cards | Anything asking for your decision: an approval, a proposed memory, a suggested model switch. See [Cards that ask for a decision](#6-cards-that-ask-for-a-decision). |
| Footer | The actions on a finished reply: **Copy**, **Regenerate** and **Retry with** the stronger model (both under the latest reply only; **Retry with** when the chat is not already on it and no switch card is waiting, since the card offers the same), **Branch**, **Save as workflow**, and **Save** (the answer, as a memory). Each is covered where it belongs later in the guide. |

Long conversations collapse their older turns behind **Show N earlier turns**,
and a minimap along the edge lets you jump between messages.

### The composer

The toolbar beneath the message box is a row of icons. Left to right:

| Control | What it does |
| --- | --- |
| Model selector | Shows the model this conversation runs on, with a cost band from `$` to `$$$$` where its pricing is known. Click to change it. See [Picking the model](#picking-the-model). |
| Persona | How the assistant thinks and writes in this chat: your deployment's personas under **Workspace**, and your own under **Yours** (see [Your own personas and digital twin](#your-own-personas-and-digital-twin)). It is chosen before the first message; once the chat is underway the control is hidden. **Manage your personas** (or **Create your own persona**) at the bottom of the menu opens **Settings → Personas**. |
| Prompt library | Opens the shared library to insert or save a prompt. See [The prompt library](#9-the-prompt-library). |
| Attach files | Add files to your next message. Dragging a file anywhere onto the conversation does the same. |
| Tools | The wrench. Its badge counts the connectors on in this conversation, **Always on** ones included; hover it for "Tools, N on". See [Connectors in a conversation](#5-connectors-in-a-conversation). |
| Context ring | Appears once the conversation has content and fills as it grows. Click it to compact. See [Long conversations](#long-conversations). |
| Auto-approve | The lightning bolt on the right. When on for this conversation, executable connector actions and shell commands run without stopping on an approval card (can be toggled mid-turn). Outbound email, scheduled-task changes, and actions the deployment requires a decision on every time still show their card. |
| Send on Enter | The toggle on the right. On, `Enter` sends and `Shift Enter` adds a line. Off, `Enter` adds a line and `⌘Enter` or `Ctrl Enter` sends. The same setting lives in **Settings**. |
| Send | At the far right. While the assistant is working it becomes **Stop**. |

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
sit at the top of the list. Typically the first is fast and inexpensive — the one
new conversations start on, and the right choice for pulls, checks, and
formatting — and the second is stronger, for judgment calls, retrospectives, and
anything where depth matters more than speed. By default those two are
**Claude Opus 5.5** (new conversations start on it) and **Gemini 4 Argon** (the
frontier fallback and alternative model); your admin can point
either slot at a different model. Your admin can also pin more models just
below those two, sometimes under their own name and with a tag such as
**alpha** that marks a model still in trial. A pinned model can also show
**Free** where its cost band would be. The rest of the list is marked by
status (**tested**, **new**, or **experimental**), and a model whose pricing the
deployment knows shows its cost band, so you can see what you are about to
spend. No band means the price is unknown rather than zero — worth asking about
before running something long on it.

The picker uses your workspace's active model providers, including providers
configured by the deployment. **Workspace** models route directly through their
named provider. Recommended and public-catalog choices that the workspace cannot
route are hidden once provider information loads. A saved conversation or default
that is no longer available stays visible with an explanation: use **Choose a
model**, pick an available workspace model, then retry. Fleet does not silently
switch an existing conversation to another provider, with one exception: a
**lockdown** conversation may only run on the models your admin allows for
lockdown (by default the workspace's default and stronger models), so if that
list changes under it, its next turn runs on the lockdown default and the
conversation keeps that model from then on. The model picker shows the switch
when the turn starts. That switch needs a specific model to move to: if your
admin has written the lockdown list entirely as patterns rather than named
models, there is nothing to switch to and the conversation keeps refusing —
start a new lockdown chat and tell your admin the list needs a named model
first. Admins can fix the provider
configuration under **Settings → Admin → Model providers**, and set the default
and stronger model under **Settings → Admin → Features → Model tiers**. Besides
OpenRouter, Anthropic, OpenAI and Ollama, that page can add any
**OpenAI-compatible** server (it needs its Base URL; a key is optional), the
**Vercel AI Gateway** (it needs a key) and an **Azure OpenAI** resource (it
needs a key, and the resource's endpoint as its Base URL; its models are the
resource's deployment names). On Azure, a GPT-5 or o-series deployment needs
its model in its name, such as `gpt-5-prod`, or its turns fail, and a newer
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
the **Workspace pack** (**Share with workspace**), click **Add to my skills**
on a teammate-shared skill in the Workspace pack to enable it for your own chats
and tasks, or click **Clone & edit** on any Workspace pack skill to load its
instructions into the editor and customize it (you can also ask the assistant in
chat to propose a skill or edit an existing one, then click **Approve** or
**Approve & share** in **Settings → Skills**).

### Your own personas and digital twin

Under **Settings → Personas** you can write personas of your own. They are
private: only chats you start can use them, and they appear under **Yours** in
the composer's persona menu.

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
  not have), the instructions, and up to 8 suggested patrols (a title, how
  often, a time zone, and what to do each time; at least 5 minutes apart).
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
| **Shared file library** | Reference files an admin has published once for every conversation to read: historical data, price lists, lookup tables. They live under **Settings → Shared files**, and the assistant sees them in every chat without anyone re-attaching them. |
| **Files the assistant makes** | Anything it produces in a conversation — a spreadsheet, a chart, an HTML report — stays in that conversation's working space. Download it from the transcript, preview HTML in place, and ask about it in later turns by name. |

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
| Selection | The **Tools** menu (the wrench) in the composer, and its library | Which of your available connectors are live in *this* conversation. Narrow it when a chat only needs one source; the assistant cannot reach what is not selected. |
| Binding | The task form in the Operations Center | Which connectors a scheduled task may use, fixed when the task is created. A later change to your preferences never rewrites an existing task. |

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
assistant cannot call a reserved tool in a conversation, whatever you turn on; a scheduled task still can, so the Operations Center task form may show
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
| Send this email? | The assistant has drafted a message and wants to send it. The card shows the sender, the subject, and the To, Cc, and Bcc recipients each on their own line, lists every file that goes out with it by name, inline images included (visible even with the body collapsed), and shows the body. | **Send** · **Cancel** |
| Email preview | The assistant wants to show you a draft without sending it. | **Dismiss** — display-only by design |
| Run this shell command? | A command the platform treats as risky. | **Approve & run** · **Cancel** |
| Schedule a task | The assistant proposes a scheduled task, usually because you chose **Make recurring task…**. See [From chat to a task](#10-from-chat-to-a-task). | **Approve & schedule** · **Edit** · **Cancel** |
| Change a task | The assistant wants to stop or update an existing scheduled task. | **Approve & stop** or **Approve & update** · **Cancel** |
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
can also turn on **Auto-approve** for the current conversation using the
lightning-bolt button in the composer toolbar (even while a reply is streaming)
so executable connector actions and shell commands run without stopping on a
card, while outbound email, scheduled-task cards, and actions that require a
decision every time still ask. Cards are not for exploring what an action would change; ask the assistant to
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
switch**, or **Dismiss**. Declining costs nothing, and after you dismiss one you
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
  **Fix** link next to a checklist line jumps to the field it is about.
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
  **Clear selection**, if the question is optional.
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
as a card for you to **Save** or **Don't save**. You can press **Save** in the
footer of any reply to keep what the assistant just said, without composing a
"remember this" turn. And the platform may propose memories it noticed on its own
after a turn, marked **Proposed** in the manager until you accept them. Nothing
enters memory without your say-so except what you typed yourself.

Open **Memories** from the header. The manager has two tabs, **My memory** and
**Team learnings**, and a **Graph** view for how entries connect. Each memory
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

**New project** (the **+** beside **Projects** in the rail) asks for a name, an
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

The rail lists every project you can see: pinned ones first, then the rest by
when each project was last created or edited (renamed, or its settings or
sharing changed) — chatting in a project does not move it up. Once the list is
longer than the space it has, the **Projects** section scrolls on its own, the
same way **Chats** does below it.

Open a project from the rail to reach its home. Its title carries a pill saying
who can see it — **Only you** or **Shared with** your team; in a shared project
the pill explains what the team sees. Until you have shared a chat in the
project, a short **getting-started card** walks the next steps (share the
project, add instructions, share a chat), each with a button that does it. The
**×** hides it for now; in a personal project, **Keep personal** hides it for
good. Then, top to bottom:

| Area | What it holds |
| --- | --- |
| **Your chats** | Your conversations in this project, with how many are shared with the team and a search box scoped to it. Each row has a pill, **Only you** or your team's name: switch it to share the chat (and its files) or to stop sharing it; **More sharing options…** opens the share dialog. In a shared project, **New chat** asks whether the new chat starts as **Only you** or shared with the team. |
| **Shared by your team** | Conversations your teammates have shared into this project, with the owner and date. Open one to read it; see [Sharing with your team](#sharing-with-your-team). A row you have already branched says **You branched this**, and still opens the owner's live chat. |
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
lighter option: tag conversations from their menu, then filter the rail by label.
Labels organize; they carry no instructions or learnings.

## 8. Keeping, shaping, and sharing conversations

Every conversation in the rail has a menu behind the `⋮` button that appears when
you hover over it. Here is everything in it.

| Item | What it does |
| --- | --- |
| **Pin** | Keep it indefinitely and show it under **Pinned**. Pin anything you would mind losing. |
| **Rename** | Conversations take a title from their first exchange. Rename the ones you keep so the rail reads as a list of work, not a list of first sentences. |
| **Move to project** | File it under a project. The conversation picks up that project's context from its next turn. |
| **Labels** | Add or remove labels. Labels double as filters in the rail. |
| **Download chat** | Export the conversation in one of three formats: **Web page**, which opens in any browser, looks like the chat, and prints to PDF; **Text document**, for pasting into email, Word, or Docs; or **Raw data**, the complete record for developers. **Include the agent's work** adds the tool calls, results, and reasoning; it is off by default so a long chat downloads as the conversation, not its machinery. |
| **Make recurring task…** | Ask the assistant to turn this conversation into a scheduled task. See [From chat to a task](#10-from-chat-to-a-task). |
| **Save as workflow** | Turn the whole conversation into a reusable workflow template in the prompt library or a personal **Agent Skill (`/skill-name`)**. See [From chat to a task](#10-from-chat-to-a-task). |
| **Share…** | Opens the share dialog: sharing with your team first, then **Share outside your team** (a public link). See [Sharing with your team](#sharing-with-your-team). |
| **Select** | Enter selection mode to pin, label, or delete several conversations at once. If the selection includes chats shared with your team, the delete confirmation says how many, and that the team loses them and their shared files; teammates who branched them keep their copies. |
| **Archive** | Put it away without deleting it. Archived conversations never expire and can be unarchived from the **Archived** group. Archiving a chat shared with your team stops sharing it (it asks first, with the number of shared files); unarchived, it comes back as **Only you**. An archived chat can't be shared with your team; unarchive it first. |
| **Delete** | Remove the conversation and everything it produced. There is a confirmation, and no undo. For a chat shared with your team, the confirmation says what the team loses; teammates who branched it keep their copies. |

> **What expires, and when.** An unpinned conversation that is not in a project,
> not archived, and not shared is deleted after a period of inactivity — 14 days
> unless your operator has set a different window. There is also a per-person cap
> on how many unpinned conversations are kept at all, so on a busy account the
> oldest can go before the clock runs out. Pinning, archiving, filing into a
> project, or sharing exempts a conversation from both.

### On the transcript

**Edit message** on any of your own messages lets you change it and resend from
that point. On any reply, the footer offers **copy** and **branch**; under the
latest reply it also offers **regenerate** (ask again, optionally on a different
model) and **Retry with** the stronger model (ask again on it, and keep it for
the rest of the chat). **Branch** starts a new, independent conversation
that begins with everything up to that reply, so you can explore an alternative
without disturbing the original. Branch when you want to keep both paths; edit
when the earlier version was simply wrong. The footer's two save actions, **Save
as workflow** and **Save**, are covered in [From chat to a task](#10-from-chat-to-a-task)
and [Memory and projects](#7-memory-and-projects).

### Sharing with your team

Sharing a conversation with your team is how a good piece of work travels. A chat
is shared with a team only when it sits in a project shared with that team **and**
its owner shares it; sharing the project alone never shares its chats.

**The share dialog** (the **Share** button in the header, or **Share…** in a
chat's menu) always leads with your team and offers one button that does the
whole job for the situation you are in:

| Situation | The button |
| --- | --- |
| The chat is in no project | **Move and share** into a project already shared with your team, or **Create shared project** if there is none yet — the chat moves in and is shared. |
| The chat is in a personal project | **Share project first**, which takes you to the project home with the confirmation open; afterwards the getting-started card offers to share the chat in one click. |
| The chat is in a project shared with your team | **Share with** your team. **Includes N files · Choose…** lets you untick files before sharing. |
| The chat is shared | **Copy link for** your team, plus **Stop sharing** (which asks first and, when it can count them, says how many files stop being shared). |
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
read-only view. Anyone else who opens it sees that the chat is shared with your
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
branch from the current version. Branch is the way to build on a colleague's
work; co-writing a live conversation is not something the platform does.

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

### Long conversations

The context ring in the composer fills as a conversation grows. With **Show
details** on, a totals line above the composer gives the same figure as a
percentage alongside the conversation's cumulative cost. When a conversation gets
full, a banner says so and the assistant starts to slow down and cost more per
turn. **Compact conversation**, from the ring or the banner, replaces the earlier
turns with a short summary so the conversation fits comfortably again; the
transcript keeps its history, only the assistant's working view is shortened.
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

There is one library, visible from two places. The book icon in the composer and
the prompt field in the Operations Center task form open the same list, so a
prompt saved from a conversation is, word for word, the prompt a task can run. It
holds two kinds of entry:

| Kind | What it is |
| --- | --- |
| Deployment prompts | Shipped with your deployment and kept up to date from outside the app. Read-only, marked **Git** in the list. Use them as they are, or insert one and adapt it into a prompt of your own. |
| Workspace prompts | Created by your team. Each is either **Private**, visible only to its author, or **Workspace**, visible to everyone on the deployment. Only the author or an admin can edit or delete one. |

### Using a prompt

Open the library, search or scroll, and select an entry to read it in full. **Use
prompt** inserts it into your draft; send it as it is, or add to it first. In the
Operations Center the same action fills the task's instructions and seeds the
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
to the **Workspace pack** so teammates can click **Add to my skills**).

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

**From the library, in the Operations Center.** The recommended route. In the
Operations Center, **New task** opens the task form; insert the prompt from the
library, then set the schedule, the model, and the recipients. The prompt has one
home and the task points at it. The Operations Center Guide covers the form field
by field.

**Make recurring task… from the chat.** The direct route. From the conversation's
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

Either way, the task appears on your own board in the Operations Center.
Teammates receive its emails, but unless they are admins they do not see its row,
so the team's version of a report should be created by whoever will maintain it.

> **One rule holds on every route.** A task keeps the prompt it was created with.
> A library edit only reaches a task when the prompt is re-selected on that task.
> After any change to a library prompt, open the tasks that use it, re-insert the
> prompt, and **Run now** once to confirm.

From here, the **Operations Center Guide** takes over: run states, logs, what to
do when a run needs attention.

## 11. When a turn goes wrong

Most problems announce themselves in the transcript. Match what you see to the
row below.

| What you see | What to do |
| --- | --- |
| **Retrying** | A connection or a service hiccupped, and the turn is retrying by itself with a short countdown. Wait. It usually clears on its own. |
| Nothing at all for a while | A model that reasons before it writes can be silent for a minute or more, and that is not a fault. By default the platform gives a model about a minute and a quarter to start. If it still has not started, you will see **Retrying** while the platform tries again by itself, and your deployment may try a backup model after that — which usually just answers, with nothing more for you to do. If the last attempt also never starts, a card says the model did not start responding. Retry from the card, or pick a different model. |
| **Turn failed** | The turn did not complete. The banner says why when it can. On the latest turn, **Retry** resends your last message. If it fails the same way twice, change something: the model, the ask, or the attachment. |
| Reply seems to stop | On a phone that locked, or a laptop that slept, the screen can lose the connection while the assistant keeps working. Wait a few seconds after the connection is back: the page re-checks by itself and either resumes the live stream or drops in the finished reply. If the turn had already failed while you were away, the page says what went wrong and offers **Retry** — or **Pick a different model**, when that is what the turn needed. After a long outage those checks space themselves out, so clicking back into the tab prompts one straight away. Refresh only if it is still stuck after that. The work was never lost either way. |
| A notice at the top: **… was updated** | A newer version of the app has been released since this page loaded. Press **Reload** when it suits you, or dismiss the notice with **×**. It never reloads the page by itself. The same notice appears on every page of the app, and also when the app is added to an iPhone or iPad Home Screen, where there is no reload button of its own. |
| A page that says **Something went wrong** | The page hit an error. Press **Reload**, or **Go to the home page**. A page left open across an update usually reloads by itself once to pick up the new version. If the same page fails again right after, it waits for you instead. |
| Model unavailable | The banner offers **Pick a different model**. Choose one and resend; the conversation keeps its history. |
| File refused | An attachment over the size limit is refused when you pick it, with the limit shown. Split the file, or trim it to the columns and dates you need. |
| Connector unavailable | The picker shows **Unavailable** against a connector, or the trail shows a call that returned an error. Check **Settings → Connections** for a sign-in that has lapsed; otherwise bring it to your administrator along with a **Download chat** export in **Raw data** format — that one always carries the full record, so it offers no options to tick. |
| Conversation is full | A banner reports the percentage. **Compact conversation**, or start a new one if the topic has moved on. |
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
| `?` | Show the shortcut help |
| `Esc` | Close search, the help overlay, or the sidebar |

Single-letter shortcuts never fire while you are typing in a text field, so they
cannot interrupt a message.

Three chords you might expect are deliberately left alone, so your browser keeps
them: `⌘F` stays find-in-page (the natural way to search the transcript in front
of you), `⌘N` stays new-window, and `⌘J` stays downloads. That is why new
conversation and focus-composer sit on the two chords above instead.
