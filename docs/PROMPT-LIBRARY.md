# Hybrid prompt library

Fleet exposes one prompt library in both interactive Chat and the Operations
Center task form. It deliberately combines two ownership models:

- **Git-backed workspace prompts** come from the active client bundle's
  `prompts/` directory. Fleet reads `.yaml`, `.yml`, `.md`, and `.txt` files
  live, preserves their exact contents, and presents them as read-only entries.
  Pulling the config repository updates the catalog without restarting Fleet.
- **Workspace prompts** are created in the UI and stored in the scheduler
  database. Their author chooses private (only the author) or workspace-shared
  (readable by authenticated workspace members). Only the author or an admin can
  edit or delete one.

The picker supports search, inserting an entry into the current chat/task draft,
creating a prompt from that draft, editing UI-owned entries, and exporting the
visible hybrid library as a versioned JSON backup. Export is intentionally a
plain file download so it can be placed in OneDrive, Dropbox, or any ordinary
backup folder without a vendor integration.

## Capturing a good chat as a workflow

The thing worth keeping off a good session is its **procedure**, not the
question that started it. A user spends an afternoon getting an agent to unzip
a client's data, profile it, compute a baseline, model an improvement and draft
the client note — what they want next quarter is that *recipe*, aimed at a
different client, not a re-run of the same analysis.

So "Save as workflow…" writes the whole conversation up as a template: a
host-side model call (`FLEET_LIBRARY_PROMPT_MODEL`) produces a draft the user
reviews and edits before it is saved through the ordinary `POST /prompts` path.
Nothing is stored until they save. Two entry points open the same dialog — the
conversation kebab, and an action in the footer of any finished assistant reply
— and **both save the entire chat**. The reply is where the reader is standing
when they decide the session was worth keeping; it is not the scope of what
gets saved.

### What the draft contains

The synthesizer is asked for a Markdown template with five sections:

| section | holds |
| --- | --- |
| **Objective** | what one run produces |
| **Inputs** | what the person must supply, each a `[BRACKETED PLACEHOLDER]` |
| **Steps** | the numbered procedure actually followed, naming the tools used at each step |
| **Output** | the deliverable's format and structure |
| **Notes** | constraints, quality bars, pitfalls, and any connector or persona the workflow depends on |

Specifics are **generalized** — this run's client names, dates, filenames and
targets become placeholders — while the method stays concrete. "Analyze the
data" is worthless in a template; "run X over Y to establish Z" is the point.
Corrections the user made mid-chat are carried forward as instructions, so the
next run starts where this one ended up instead of repeating its mistakes.

### Why it reads the tool calls

`workflowTranscriptFromHistory` (`internal/httpapi/`) renders the conversation
for this synthesizer, and it deliberately differs from the one
promote-to-task uses. That one keeps the user/assistant **text** turns, which
is right: a recurring task needs the ask. A workflow needs the **method**.

The proportions make the case. The conversation this was built against is 289
history entries: 11 user turns, 168 assistant entries, 110 tool calls and
results. Feeding the text turns alone hands the synthesizer a small fraction of
the run and asks it to describe a procedure it cannot see — which is exactly
how an earlier version produced a single restated question instead of a recipe.

The renderer keeps asks, answers and every tool call in order, and:

- **collapses consecutive calls to the same tool** into one line with a count
  (`[tool: bash ×40]`) — "ran bash forty times" is the reusable signal, forty
  near-identical lines is that signal at forty times the cost;
- **omits successful tool results**, which are the run's *data* and the single
  largest thing in a transcript, while **keeping failures** (`[tool error: …]`)
  because what went wrong mid-run is exactly what a template should warn about;
- **omits reasoning**, which is the model's private deliberation rather than a
  step of the workflow;
- **bounds** each turn and each tool input, so one pasted dataset cannot crowd
  the rest of the run out of the budget.

Conversation setup the transcript cannot show — title, persona, and the
optional MCP connectors the chat had enabled — is passed alongside it, so a
workflow that depends on a connector says so in its Notes rather than failing
for the next person.

When the transcript exceeds its budget, **both ends are kept** and the middle
is dropped (`keepTranscriptEnds`). The recurring-task path keeps only the tail,
because the refined ask lives at the end of an exploration; a workflow's
opening turns carry its objective and inputs, so losing them costs the template
its first two sections. The middle of a long run is its most repetitive part.

The call is metered like the recurring-task synthesizer
(`AuxUsageLibraryPromptSynthesis`): a conversation-level user action with no
run session, so the structured host log line is the whole record of what it
cost.

## Bundle format

Create `<bundle>/prompts/` and add prompt files. For YAML, top-level `name`,
`description`, and `goal` fields provide catalog metadata; the full YAML remains
the inserted prompt. For Markdown, the first level-one heading is the name and
the first prose line is the description. Otherwise Fleet derives the name from
the filename. `README.*`, symlinks, unsupported extensions, invalid UTF-8,
files over 256 KiB, and entries past the 256-file catalog limit are skipped.

```yaml
name: Weekly project brief
description: Summarize progress, risks, decisions, and next steps.
inputs:
  - project notes
  - issue tracker updates
instructions:
  - Cite the source for each claim.
  - Call out owners and due dates for every next step.
```

Git entries are trusted workspace content, like personas and protocols. They do
not grant tools or permissions: selecting one only fills the ordinary composer
or task prompt, and the existing create/run governance still applies.

## API

The authenticated Operations API exposes:

- `GET /prompts` — merged visible catalog.
- `POST /prompts` — create a private or workspace prompt.
- `PUT /prompts/{id}` / `DELETE /prompts/{id}` — owner/admin mutations for
  database-backed entries.
- `GET /prompts/export` — versioned JSON export.

Git entries have `source: "git"` and `read_only: true`; UI-owned entries have
`source: "workspace"`, their visibility, and an `owned_by_caller` affordance.

## Shipped scope and deliberate deferrals

- Shipped: live Git catalog, shared picker on both surfaces, private/shared UI
  storage, CRUD, search, exact-content insertion, JSON backup, and the
  save-as-workflow capture above from both entry points.
- Deliberately NOT shipped: saving one exchange rather than the session. It was
  built that way first and was wrong — it keeps the answer and loses the
  method. A chat carrying two unrelated workflows is better handled by editing
  the draft, or by branching the chat, than by a scope picker nobody would
  reach for.
- Deferred: writing back into Git (Fleet never mutates the external bundle),
  automatic cloud-drive sync, prompt version history, and JSON re-import. The
  exported format is versioned so import can be added compatibly later.
