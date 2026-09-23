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

If the server restarts during an approved action, its outcome is shown as unknown;
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
team. See [Keeping, shaping, and sharing conversations](#8-keeping-shaping-and-sharing-conversations).

Your account menu at the foot of the rail switches between **Light**, **Dark**,
and **System** appearance and opens **Settings**.

Administrators manage account access under **Settings → Admin → Users**. Chat
defaults to **Contributor**; **Viewer** is read-only. **Fleet Admin** grants full
Chat and Ops Center permissions and highlights the included Contributor choices.
Selecting Fleet Admin again turns it off and safely defaults to Chat Contributor
with no Ops access. Accounts created and granted Fleet in central Auth are added
here automatically with the roles selected there.

### The header

Above the transcript sit three small controls, left to right: **Keyboard
shortcuts** shows the same list as the [last section](#13-keyboard-shortcuts) of
this guide; **Memories** opens the memory manager; and **Show details** toggles
the assistant's reasoning, tool calls, and per-turn cost figures on or off across
the whole conversation. A conversation you have shared with your team also shows
a **Shared with team** chip here, which opens the share dialog.

### The transcript

Your own messages appear as you typed them. When the platform adds something to a
message that you did not type, such as an attached file or a note from the shared
file library, it appears as a collapsed note beside your message rather than
inside it, so what you wrote and what was added stay visibly separate.

Each reply from the assistant is laid out in a fixed order, top to bottom, so
your eye lands on the answer and then on anything that needs you:

| Part | What it is |
| --- | --- |
| Execution trail | A row of chips, one per tool the assistant used: a file it read, a connector it called, code it ran. Click a chip to see exactly what went in and what came back. This is how you check the work. Shown only when **Show details** is on. |
| Reasoning | The assistant's narrated thinking. Also shown only when **Show details** is on. |
| The answer | Text, tables, charts, and images. Code and data come in copyable blocks. When the assistant produces an HTML report, it renders as an **HTML preview** with a **Show source** toggle. |
| Status | Banners for anything unusual about the turn: retrying, cancelled, failed. See [When a turn goes wrong](#11-when-a-turn-goes-wrong). |
| Cards | Anything asking for your decision: an approval, a proposed memory, a suggested model switch. See [Cards that ask for a decision](#6-cards-that-ask-for-a-decision). |
| Footer | Five actions on a finished reply: **Copy**, **Regenerate**, **Branch**, **Save as workflow**, and **Save** (the answer, as a memory). Each is covered where it belongs later in the guide. |

Long conversations collapse their older turns behind **Show N earlier turns**,
and a minimap along the edge lets you jump between messages.

### The composer

The toolbar beneath the message box is a row of icons. Left to right:

| Control | What it does |
| --- | --- |
| Model selector | Shows the model this conversation runs on, with a cost band from `$` to `$$$$` where its pricing is known. Click to change it. See [Picking the model](#picking-the-model). |
| Persona | A saved working style for the assistant, shown when your deployment ships personas. **Default** is right for nearly everything. |
| Prompt library | Opens the shared library to insert or save a prompt. See [The prompt library](#9-the-prompt-library). |
| Attach files | Add files to your next message. Dragging a file anywhere onto the conversation does the same. |
| Connectors | Shows a count of the connected sources live in this conversation. See [Connectors in a conversation](#5-connectors-in-a-conversation). |
| Context ring | Appears once the conversation has content and fills as it grows. Click it to compact. See [Long conversations](#long-conversations). |
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
**GPT-6 Luna Pro** (new conversations start on it) and **Claude Opus 5.5** (the
model a "switch to a stronger model" suggestion offers); your admin can point
either slot at a different model. The rest of the list is marked by
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
and stronger model under **Settings → Admin → Features → Model tiers**.
If a direct provider takes precedence over OpenRouter, available OpenRouter
choices are labeled with their configured provider name and use an explicit
provider-prefixed route.

The assistant will sometimes suggest a switch itself: a card offering **Switch &
retry** when a question is heavier than the current model handles well, or a
one-line banner when you attach a large spreadsheet to the fast model. Both are
suggestions. Nothing changes unless you accept.

### Personas and skills

A **persona** is a saved working style your deployment may ship, chosen from the
composer. A **skill** is a packaged capability the assistant can pick up on its
own when a task fits, or that you can invoke directly by typing `/` at the very
start of a message and choosing from the list. The platform ships a small pack
of general-purpose skills, and your deployment can add its own.

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
The connector's existing permissions and publication checks still apply; a
staged upload alone does not mean a page or document was published.

## 5. Connectors in a conversation

A connector is a live link to a system: a mailbox, a reporting API, a document
store. Turning one on has two layers in Chat, and a third in the Operations
Center.

| Layer | Where | What it decides |
| --- | --- | --- |
| Availability | **Settings → Connections** | Which connectors exist for you at all, whether each is on by default in new chats, and which account it uses when a connector has more than one. Set once, lasts. |
| Selection | The **Connectors** picker in the composer | Which of your available connectors are live in *this* conversation. Narrow it when a chat only needs one source; the assistant cannot reach what is not selected. |
| Binding | The task form in the Operations Center | Which connectors a scheduled task may use, fixed when the task is created. A later change to your preferences never rewrites an existing task. |

Some connectors are marked **Always on**: they are part of the deployment and
cannot be switched off per conversation. Each connector is also badged as
**Bundled**, shipped and operated with the deployment, or as a third-party
service you have signed into yourself, so you always know whose system you are
reaching. A **Beta** badge means it works but still has rough edges.

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
| Send this email? | The assistant has drafted a message and wants to send it. The card shows sender, recipients, subject, and body. | **Send** · **Cancel** |
| Email preview | The assistant wants to show you a draft without sending it. | **Dismiss** — display-only by design |
| Run this shell command? | A command the platform treats as risky. | **Approve & run** · **Cancel** |
| Schedule a task | The assistant proposes a scheduled task, usually because you chose **Make recurring task…**. See [From chat to a task](#10-from-chat-to-a-task). | **Approve & schedule** · **Edit** · **Cancel** |
| Change a task | The assistant wants to stop or update an existing scheduled task. | **Approve & stop** or **Approve & update** · **Cancel** |
| Run an action | Any other connector action that changes something, such as updating a record in an external system. The card names the action, the system it runs against, and the arguments it was called with. | **Approve & run** · **Cancel** |

Three rules for every card. **Read what is about to happen**, not just the
button: the card exists to tell you. **Cancel is free**: the assistant continues
without doing the thing, and you can ask it to try a different way. **Cards
expire**: a card left unanswered times out to no, and the transcript records that
it did.

After you approve, the card shows what actually happened: it ran, it failed, it
is still running, or — for an older card whose run finished before outcomes were
stored — that the outcome was not recorded. A still-running card is not a new
decision: Cancel, Edit, and the expiry countdown are withheld, and **Check
result** fetches the outcome without running the action twice. In terminal chat
the same flags apply: `/approve <id>` on a running card retrieves the result;
an unknown historical outcome is an error, not success.

Some cards carry a checkbox that widens your decision to every later call of the
same kind in this conversation, turning the button into **Approve + allow all**.
Leave it unticked unless you know exactly what the rest of the conversation will
do. Cards are not for exploring what an action would change; ask the assistant to
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
well, a card offers **Switch & retry**, **Just switch**, or **Dismiss**.
Declining costs nothing.

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

**Create project** from the **Projects** group in the rail. A project has a name,
standing instructions, and one switch: **Share with my team**. Off, the project
is personal and only you see it. On, everyone on your team sees it in their rail
and can work inside it. You need to be on a team for the switch to be available;
teams are set up in **Settings → Team** and members are added by an admin. The
same settings are reachable later from **Project settings** on the project home.

The project's owner can hand it over with **Transfer** to another member, so a
project outlives its creator's involvement. Deleting a project asks first and
names what goes with it: its team learnings, and how many members' chats leave it
and become temporary again. Turning **Share with my team** off asks too, and
quotes how many teammates' chats it will unfile back into their own Temporary
lists. Nothing in either case deletes a conversation; chats always stay with
their owner.

### The project home

Open a project from the rail to reach its home. Left to right, top to bottom:

| Area | What it holds |
| --- | --- |
| **Chats** | Your conversations in this project, with a search box scoped to it. Chats you have shared with the team carry a badge here. |
| **Shared by your team** | Conversations your teammates have shared into this project. Open one to read it; see [Sharing with your team](#sharing-with-your-team). Until someone shares, it says so and reminds you that your own shared chats stay in your list above. |
| **Instructions** | The standing instructions. Editable by the owner. |
| **Team learnings** | One row per learning, each with who wrote it and when. **New team learning** adds one. The row menu holds **Pin**, **Edit**, **Retire** or **Restore**, and **Delete**. The same list is the Team learnings tab of the memory manager, so you can tend it without leaving a conversation. |
| **Sources** | Files from your own conversations in this project. Files are never shared through a project; a teammate's files stay theirs. |

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
| **Save as workflow** | Turn the whole conversation into a reusable workflow template in the prompt library. See [From chat to a task](#10-from-chat-to-a-task). |
| **Share** | Opens one dialog with two audiences. **Share by link** creates a read-only link anyone can open: **Create link**, **Copy link**, **Stop sharing**. **Share with team** makes the conversation readable by your teammates, and is available only when the chat is inside a project shared with your team; if it is not, the dialog says so and offers **Move to project** right there. A chat can be shared both ways, either way, or neither, and each shows its own badge in the rail. |
| **Select** | Enter selection mode to pin, label, or delete several conversations at once. |
| **Archive** | Put it away without deleting it. Archived conversations never expire and can be unarchived from the **Archived** group. |
| **Delete** | Remove the conversation and everything it produced. There is a confirmation, and no undo. |

> **What expires, and when.** An unpinned conversation that is not in a project,
> not archived, and not shared is deleted after a period of inactivity — 14 days
> unless your operator has set a different window. There is also a per-person cap
> on how many unpinned conversations are kept at all, so on a busy account the
> oldest can go before the clock runs out. Pinning, archiving, filing into a
> project, or sharing exempts a conversation from both.

### On the transcript

**Edit message** on any of your own messages lets you change it and resend from
that point. On any reply, the footer offers **copy**, **regenerate** (ask again,
optionally on a different model), and **branch**: a new, independent conversation
that starts with everything up to that reply, so you can explore an alternative
without disturbing the original. Branch when you want to keep both paths; edit
when the earlier version was simply wrong. The footer's two save actions, **Save
as workflow** and **Save**, are covered in [From chat to a task](#10-from-chat-to-a-task)
and [Memory and projects](#7-memory-and-projects).

### Sharing with your team

Sharing a conversation with your team is how a good piece of work travels. Two
things to know about what it shares. It shares the **transcript only**: your
messages and the assistant's replies. Tool calls, tool results, reasoning, and
attachments are left out, and files the assistant produced stay behind your own
account, so a conversation about a report does not hand out the report. And it
shares **read-only**: a conversation has exactly one owner, and nothing a
teammate does changes yours.

A team-shared conversation always has a home: the project it sits in. If the chat
is moved out of the project, the project is made personal or deleted, or you
leave the team, the share ends with it. You can also end it yourself at any time
from the same dialog.

**Reading a teammate's chat.** Open it from **Shared by your team** on the
project home. You get the transcript with a banner naming the owner and the team,
and one action where the composer would be: **Branch to continue in your own
chat**. That creates a new conversation of your own, filed into the same project,
starting with everything in the transcript up to that point. It is private until
you share it, and it is unaffected if the original is later unshared or deleted.
Branch is the way to build on a colleague's work; co-writing a live conversation
is not something the platform does.

### While a turn is running

**Stop** halts the current turn; the partial reply stays in the transcript.
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
When a topic changes entirely, a new conversation is cheaper and clearer than
compacting an old one.

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

### Creating and editing

**New prompt** opens a form with a name, an optional description ("helps
teammates find it"), and the prompt text. If you already have text in the
composer, it is carried into the form. **Share with this workspace** decides
Private or Workspace; leave it unticked until the prompt has been reviewed and is
the team's agreed version. Your own entries offer **Edit** and **Delete** when
selected.

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
method stays specific. You review the draft in the save form before it enters the
library.

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
| **Turn failed** | The turn did not complete. The banner says why when it can. **Retry** resends your last message. If it fails the same way twice, change something: the model, the ask, or the attachment. |
| Reply seems to stop | On a phone that locked, or a laptop that slept, the screen can lose the connection while the assistant keeps working. Wait a few seconds after the connection is back: the page re-checks by itself and either resumes the live stream or drops in the finished reply. If the turn had already failed while you were away, the page says what went wrong and offers **Retry** — or **Pick a different model**, when that is what the turn needed. After a long outage those checks space themselves out, so clicking back into the tab prompts one straight away. Refresh only if it is still stuck after that. The work was never lost either way. |
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
