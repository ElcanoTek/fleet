# Email templates library

Fleet shows a client bundle's **email templates** in Chat: the fixed shapes of
the emails a team sends again and again (a weekly client update, a reporting
request, an internal brief). Each template is shown as the email it produces
beside the rules for every section, and inserts a prompt that asks the
assistant to write the email to that template.

It sits beside the [prompt library](PROMPT-LIBRARY.md) and follows the same
ownership model for bundle content: files in the bundle, read live, read-only
in the UI. A team can also add its own from chat; see
[Team templates](#team-templates).

## Why

People were keeping one long chat alive per client because it was the only way
to get this week's update in the same shape as last week's. A fresh chat
produced an email three times as long with different sections. A prompt alone
did not fix that, because a prompt is guidance and a long transcript outweighs
it. The fix has two halves, and fleet owns one of them:

- **The bundle enforces the template.** Its mail connector checks the email's
  blocks against the template when it renders, and refuses a misfit with the
  list of what to fix. That is bundle code (in the Elcano bundle,
  `render_email(template=...)`), not fleet's.
- **Fleet makes the template visible and easy to ask for.** That is this
  library.

## What shipped

**Bundle format.** `<bundle>/email_templates/<id>.yaml`, one file per template,
and an optional `<bundle>/email_templates/previews/<id>.html` beside them: the
bundle's own rendering of the template's sample. Fleet reads these keys and
ignores the rest (the bundle's connector owns the full schema and validates it
strictly):

| key | use |
| --- | --- |
| `name`, `description`, `use_when`, `based_on` | catalog text |
| `audience` | `client` or `internal`; shown as a badge |
| `max_words` | cap on the prose of the whole email |
| `subject.instruction`, `subject.max_chars` | the subject-line rule |
| `rules` | writing rules, shown as a list |
| `never_say` | phrases the renderer refuses; a `re:` entry is a regular expression |
| `sections[]` | `id`, `title`, `heading`, `block`, `required` (default true), `instruction`, `limits` (a map of name to whole number) |
| `fields`, `promptTemplate` | an optional form, in the [form-prompt](PROMPT-LIBRARY.md#form-prompts) schema |

`README.*`, symlinks, non-YAML files, files over 256 KiB and entries past 64
are skipped. A file that does not parse, or has no name or no usable section,
is left out and reported; a form that fails validation is dropped and the
template is still served; a preview over 1 MiB, or one that is not a regular
file (a symlink, say), is not served. Problems go to the server log on each read, as the prompt
library's do.

**API.** `GET /email-templates` on the Operations API (browser path
`/api/orchestrator/email-templates`) returns the templates sorted by name, each
with its sections, rules and form, and `has_preview`. It never carries a
preview, so it stays small: the composer reads it to decide whether to show its
menu entry. `GET /email-templates/{id}` returns one template with
`preview_html`; the id is matched against the templates the bundle holds, never
used as a path. Both are read-only and need the view permission. Like `/prompts` it is shared with chat, so a signed-in
member without Operations Center access reads it by their Fleet role.

**UI.** Chat's composer menu (**More options**) lists **Email templates** when
the workspace has any (the bundle's or the team's); the composer asks when the
menu opens, keeps a yes, and asks again after a no, so a team's first template
brings the entry in without a reload. The
dialog reads the example of the template on screen, one at a time. It has
three columns: the list with search, the example email, and the
template's rules (sections in order with their limits in words, then the rules
for the whole email and the phrases it never uses). **Use this template** opens
the template's form in place of the rules, and **Use prompt** puts the rendered
`promptTemplate` in the composer. A template with no form inserts a one-line
request naming the template instead.

A template's form can use the two [pickers](PROMPT-LIBRARY.md#pickers): a
`recipients` field for who the email goes to (the addresses the person mails
most are one click each) and a `palette` field for how it looks (the bundle's
themes as swatches, or custom colors, with the ones used last offered first).
Both only add a line to the prompt; the bundle's renderer is what applies and
checks them.

**The example takes the picked look.** When the bundle ships
`email_templates/previews/styles.json`, the single-template read returns it as
`preview_styles`: for each value of the form's `palette` field, which #RRGGBB
colors of the preview become which (`options`), and for each custom color, the
preview colors it replaces (`custom`). As the person picks a style, the dialog
swaps those colors in the preview markup in one pass
(`web/src/app/shared/lib/emailPreviewStyle.ts`). Fleet does not know what a
theme is; the table is the bundle's, and a table holding anything but #RRGGBB
values is reported and not served, since its values are written into the
preview.

**Nothing is sent without the approval card.** Use prompt never sends anything:
it fills the composer, and the person sends the message. When the assistant
then calls `send_email`, a chat stages that call on an approval card that shows
the email and names the recipients, and it goes out only when the person
chooses Send ([AGENT-RUNTIME](AGENT-RUNTIME.md)). The exception is a
conversation where the person themselves pre-approved sending. So a bundle's
`promptTemplate` can end by telling the assistant to call `send_email` as soon
as the email is built: the card is the review step, and a display-only
`preview_email` first only adds a turn.

**The preview is untrusted markup.** It comes from the bundle, which is trusted
content, but it is an HTML document all the same, so it is shown in an
`<iframe sandbox="">` via `srcdoc`: no scripts, no same-origin access, no
navigation. The page's CSP already allows the `data:` images an email preview
carries.

## What fleet does not do

- **It does not enforce a template.** Fleet never renders or checks an email.
  A bundle whose mail connector ignores templates gets a library that only
  inserts prompts. The UI says emails that break the rules are not built; that
  is true only of a bundle that enforces them, so ship the library and the
  enforcement together.
- **It does not generate the preview.** A bundle that ships no preview file
  shows the rules without an example.
- **Limits are displayed by name.** The library words the limit names it knows
  (`max_words`, `max_paragraphs`, `max_bullets`, `min_bullets`,
  `max_words_per_bullet`, `max_items`, `min_items`, `max_rows`, `max_columns`)
  and leaves out one it does not, rather than showing a raw key.

## Team templates

A person can add a template from chat ("save this email as a template"). Fleet
does not write templates: the bundle's mail connector does, with its own
validation and its own rendering of the example, behind one of its
approval-gated tools. Fleet's part is the place and the reading.

**The place.** `<workspace-root>/mcp-runs/email-templates/`
(`agentcore.TeamEmailTemplatesDir`), in the bundle directory's own layout:
`<id>.yaml`, `previews/<id>.html`, `previews/styles.json`. A bundle hands it to
its connector as

```yaml
env:
  EMAIL_TEMPLATES_TEAM_DIR: "${FLEET_WORKSPACE_ROOT}/mcp-runs/email-templates"
```

It sits under `mcp-runs/` because that tree is already mounted read-only in
every sandbox, on both backends: host-side connectors write there by pathname
and no `bash` or `run_python` can. A saved template is shown to everyone and
its rules become instructions to later chats, so the approval card on the
connector's save tool must be the only way in. The name cannot collide with a
run directory (`task-<id>`).

**The reading.** `clientconfig.EmailTemplateLibrary` serves the bundle's
templates and the team's as one list. A team entry has `source: "team"`,
`created_by` and `created_at` (what the connector recorded; a bundle template
never carries them), and `path: team/<file>`. The team directory is read with
the bundle directory's bounds and refusals (regular files only, size caps, an
id matched against the listing and never joined into a path), a directory that
is a symlink is not read, and a team file under a bundle template's id is
reported and not served.

**The UI.** A team template has a **Team** badge and an "Added by" line. **New
template** puts a request in the composer for the assistant to save an email
from the chat as a template; like every library action it sends nothing.

## Deliberately deferred

- Editing a template in the library itself. A team template is added, replaced
  or removed from chat (above), because its example has to be rendered and
  checked by the bundle; the library stays a reader.
- Per-person or per-team visibility. A team template is for everyone on the
  deployment.
- Backing up team templates. They live under the workspace root, not in the
  database; a template worth keeping for good belongs in the bundle's
  repository.
- The library in the Operations Center task form. The inserted prompt works in
  a task, but the entry point is chat only for now.
- Translating a template's rules. Names, descriptions and forms go through the
  bundle's translations; the section rules stay in the language the emails are
  written in.
- `fleet validate-config` does not check the email template directory.
- The generic `config/default` bundle ships no email template, because it has
  no mail connector that would enforce one.
