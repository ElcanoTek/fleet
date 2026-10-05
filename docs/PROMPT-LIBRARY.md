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
filling in a Git entry's [form](#form-prompts) instead of editing placeholders by
hand, creating a prompt from that draft, editing UI-owned entries, and exporting
the visible hybrid library as a versioned JSON backup. Export is intentionally a
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
the inserted prompt, unless the file declares a [form](#form-prompts). For Markdown, the first level-one heading is the name and
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

A YAML prompt can also declare a form; see [Form prompts](#form-prompts).

Git entries are trusted workspace content, like personas and protocols. They do
not grant tools or permissions: selecting one only fills the ordinary composer
or task prompt, and the existing create/run governance still applies.

## Form prompts

A prompt that needs inputs used to carry them as `[PLACEHOLDERS]`: the user
inserted the prompt, then had to find and overwrite each one inside a large text
box, and usually missed one. A Git YAML prompt can instead declare a **form**.
Picking it in the library shows the form where a plain entry shows its text;
**Use prompt** renders the prompt from the answers and inserts it into the chat
composer or the task's prompt, exactly where a plain entry goes, so it can still
be read, edited and given attachments before anything runs.

```yaml
---
name: "New campaign page from a template"
description: "A campaign dashboard built from the partner's Pages template and filled with real data."
mode: interactive
fields:
  - key: partner
    label: Partner
    type: select
    required: true
    default: TWC
    options: [TWC, RainBarrel, Reklaim, Raptive, Outcomes CA, Other]
  - key: campaign
    label: Campaign name
    type: text
    required: true
    placeholder: Go Raw CTV
  - key: kpis
    label: Channel(s) and KPI target
    type: textarea
    required: true
    placeholder: "CTV, CPM $27"
  - key: deals
    label: Deals
    type: textarea
    advanced: true
  - key: shareable
    label: Client-shareable (adds a password)
    type: toggle
    advanced: true
    default: false
promptTemplate: |-
  Create a new Pages dashboard by following protocols/page-creation.md, route A (from a template).
  Partner: {partner}
  Campaign: {campaign}
  Channels and KPI targets: {kpis}
  Deals: {deals}
  Client-shareable: {shareable}
```

Filling in TWC, "Go Raw CTV" and "CTV, CPM $27" and leaving the optional fields
alone inserts:

```text
Create a new Pages dashboard by following protocols/page-creation.md, route A (from a template).
Partner: TWC
Campaign: Go Raw CTV
Channels and KPI targets: CTV, CPM $27
Client-shareable: no
```

The generic bundle ships a working example,
`config/default/prompts/meeting-follow-up.yaml`, that uses every field type.

### The keys

Two top-level keys make a form; every other key in the file (`name`,
`description`, `mode`, anything else) is left alone:

- `fields` — a list of inputs, in the order they appear.
- `promptTemplate` — the text to insert, where each `{key}` token is replaced by
  that field's answer.

Each field uses the schema the chat empty-state cards (`empty_state.cards[]` in
the manifest) already use, and is drawn by the same components:

| property | meaning |
| --- | --- |
| `key` | Required. Letters, digits and underscores, unique in the form; `{key}` in the template names it. |
| `label` | Required. The field's label. |
| `type` | Required. `text`, `textarea`, `select`, `number`, `daterange` or `toggle`. |
| `required` | `true` disables **Use prompt** until the field is filled (a daterange needs both dates). Not allowed on a toggle, or together with `advanced`. |
| `placeholder` | Example text shown in an empty text, textarea or number input. |
| `hint` | Help text under the input. |
| `default` | The starting value: a string for text and textarea, one of `options` for a select (otherwise the first option is preselected), a number for a number (otherwise `min`, or 0), `true`/`false` for a toggle (otherwise off), and `{from, to}` YYYY-MM-DD dates for a daterange. |
| `options` | A select's choices (strings or numbers). Required on a select, not allowed elsewhere. |
| `advanced` | `true` tucks the field under a collapsed **More options** toggle, which summarises the current values while closed. Use it for optional fields. |
| `min` | A number field's minimum. |

### How the prompt is rendered

The template is rendered with the cards' interpolation — a select, text,
textarea or number gives its trimmed value, a daterange gives `from → to` (`?`
for a missing end), a toggle gives `yes` or `no` — plus one rule the cards do not use: **a template
line whose tokens were all left blank is dropped**. An optional field the user
skipped therefore vanishes instead of leaving `Deals: {deals}` behind. A line
with at least one answered token, or with no token at all, is kept, and a blank
token on a kept line stays as written. So put each optional field on a line of
its own. A toggle always has a value, so its line is always kept.

The form shows a live **Prompt preview** of exactly what **Use prompt** will
insert. **Insert raw prompt** inserts `promptTemplate` itself, tokens intact,
without filling anything in, for someone who would rather edit the text by
hand. In the Operations Center either action also seeds an empty task title
from the prompt's name, as a plain entry does.

### Validation, and what happens when it fails

Fleet checks the form every time it reads the catalog. Besides the property
rules in the table, every `{token}` in `promptTemplate` must be a declared
`key`, every field must be used by at least one token, unknown field properties
(a slip such as `require` for `required`) are rejected rather than ignored, and `fields` and
`promptTemplate` must come together.

A form that fails any check **does not break the library**. The entry is served
as an ordinary plain prompt — its raw file, inserted as before — and the reason
is reported through the catalog's problems list, which fleet logs on every read
of the library:

```text
prompt library: prompt new-campaign.yaml: invalid form, served as a plain prompt: fields[1] (campaign): unknown property "require"; promptTemplate uses {deal}, which is not a declared field key
```

That makes the reserved names a compatibility rule worth knowing: a YAML prompt
written before forms existed that happens to have a top-level `fields` key is
still served exactly as before, with one such log line. Markdown and text prompts
never have forms. A YAML file that does not parse is served as plain text as it
always was; it is reported only when it visibly tries to declare a form.

`content` stays the raw file, form definition included, so **Back up JSON**, the
export endpoint and any copy of the library carry the prompt exactly as Git
tracks it.

## API

The authenticated Operations API exposes:

- `GET /prompts` — merged visible catalog.
- `POST /prompts` — create a private or workspace prompt.
- `PUT /prompts/{id}` / `DELETE /prompts/{id}` — owner/admin mutations for
  database-backed entries.
- `GET /prompts/export` — versioned JSON export.

Git entries have `source: "git"` and `read_only: true`; UI-owned entries have
`source: "workspace"`, their visibility, and an `owned_by_caller` affordance.
A Git entry with a valid form also carries `fields` (the field list above, as
JSON) and `prompt_template`; both are omitted for every other entry, so a client
that does not know about forms keeps inserting `content`.

## Shipped scope and deliberate deferrals

- Shipped: live Git catalog, shared picker on both surfaces, private/shared UI
  storage, CRUD, search, exact-content insertion, JSON backup, and the
  save-as-workflow capture above from both entry points.
- Deliberately NOT shipped: saving one exchange rather than the session. It was
  built that way first and was wrong — it keeps the answer and loses the
  method. A chat carrying two unrelated workflows is better handled by editing
  the draft, or by branching the chat, than by a scope picker nobody would
  reach for.
- Shipped with form prompts: `fields` + `promptTemplate` on Git YAML prompts,
  validated at catalog read with a plain-prompt fallback, rendered by the
  empty-state cards' field components (moved to `web/src/app/shared/ui/FormFields.tsx`
  so both surfaces share one copy), the blank-line rule, and **Insert raw
  prompt**.
- Deferred with form prompts: forms on workspace (UI-authored) prompts — the
  database model and the create/edit dialog have no field list yet, so a form
  is Git-only; a form for Markdown or text prompts; and conditional fields
  (show one field only for a given answer to another). Invalid forms are
  reported in the server log only — `fleet validate-config` does not check the
  prompt library yet.
- Deferred: writing back into Git (Fleet never mutates the external bundle),
  automatic cloud-drive sync, prompt version history, and JSON re-import. The
  exported format is versioned so import can be added compatibly later.
