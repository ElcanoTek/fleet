# Email templates library

Fleet shows a client bundle's **email templates** in Chat: the fixed shapes of
the emails a team sends again and again (a weekly client update, a reporting
request, an internal brief). Each template is shown as the email it produces
beside the rules for every section, and inserts a prompt that asks the
assistant to write the email to that template.

It sits beside the [prompt library](PROMPT-LIBRARY.md) and follows the same
ownership model for bundle content: files in the bundle, read live, read-only
in the UI.

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
the bundle has any; the composer asks once, the first time the menu opens. The
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

**Draft first.** Use prompt never sends anything: it fills the composer, and
the person sends the message. The assistant then builds the email as a file and
shows it. Sending is a separate `send_email` call, and in a chat every such
call is staged on an approval card that names the recipients and waits for the
person to choose Send ([AGENT-RUNTIME](AGENT-RUNTIME.md)). The exception is a
conversation where the person themselves pre-approved sending. A bundle should
still end its `promptTemplate` with the instruction to show the draft and wait,
so the assistant does not stage a send the person did not ask for yet.

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

## Deliberately deferred

- Templates created or edited in the UI. Like form prompts, they are Git-only:
  there is no database model for them, and an editable template would need its
  preview rendered by the bundle.
- The library in the Operations Center task form. The inserted prompt works in
  a task, but the entry point is chat only for now.
- Translating a template's rules. Names, descriptions and forms go through the
  bundle's translations; the section rules stay in the language the emails are
  written in.
- `fleet validate-config` does not check the email template directory.
- The generic `config/default` bundle ships no email template, because it has
  no mail connector that would enforce one.
