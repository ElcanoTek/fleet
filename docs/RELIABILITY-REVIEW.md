# Repository reliability review

This review follows the path from project sharing, choosing files and editing a
task to executing it and retrieving its output. It repairs existing behavior and completes the
rerun attachment and shared-dialog keyboard interactions. It does not change the
mandatory sandbox, broker authorization, or bundle boundaries.

## Confirmed problems and fixes

- **Workspace attachment staging:** an agent-created symlink in `attachments/`,
  the upload directory, or the destination filename could redirect a host-side
  copy outside the conversation workspace. Directory creation, opens, existence
  checks and cleanup now use an `os.Root` anchored to that workspace. Three
  regression cases fail against the original implementation.
- **Artifact downloads:** lexical/symlink validation followed by an ordinary
  `os.Open` left a filesystem race. The final open is now rooted, and metadata
  comes from the opened descriptor. Artifact URLs can be overwritten by later
  work, so responses use private revalidation instead of a day of immutable
  caching.
- **Shared-file quotas:** concurrent requests could all pass the size check
  before taking the write lock. Quota admission and persistence now share the
  lock. The regression submits eight 300 KiB files under a 1 MiB cap and verifies
  that only three succeed.
- **Task attachments:** failed uploads are retried on submission instead of
  silently omitted; duplicate and invalid files do not consume available slots.
  Rerun/clone APIs accept validated replacement `files` and `file_names`.
  Omission inherits existing attachments; an explicit empty array clears them.
  The editor explains replacement and protects attachment-only edits from
  accidental dismissal.
- **Reasoning budgets:** task editing initializes the saved budget, includes it
  in unsaved-change detection, uses `thinking_budget_tokens` on rerun, and can
  explicitly clear the override to inherit the default.
- **Recurring CLI batches:** recurring entries without an explicit start now
  resolve their first cron occurrence instead of starting immediately. Invalid
  timezones and schedules with no future occurrence fail validation.
- **Scheduler promotion:** the database update rechecks that a selected task is
  still due, ungated, and a cron task. Postponement, adding a `run_if` condition,
  or conversion to a webhook template now wins over a stale scheduler selection.
- **Reply readability (Projects pass 2, #22):** remove the global composer fade
  and restore the original transcript spacing. The team-view branch CTA owns
  its own treatment; normal chats receive no gradient, mask, or added padding.
- **Branched transcripts (Projects pass 2, #11):** Markdown renderer component
  identities remain stable across transcript rerenders. Failed workspace images
  no longer remount and retry on every scroll/measurement update, and HTML
  preview source toggles retain their state. An effective image-URL change still
  creates fresh image state. The backend continues withholding parent artifacts;
  files are not copied to work around the display bug. The browser fixture
  reproduced 228 repeated image requests before the fix; afterward there were
  none during 90-frame samples on both load and reload.
- **Project confirmations (Projects pass 2, #23):** the shared confirmation body
  uses normal prose flow instead of a grid. Team/project chips have their own
  badge treatment and keep trailing punctuation attached in move, remove,
  delete-project, transfer, and unshare confirmations.
- **Mobile project navigation:** finishing a background conversation restore no
  longer closes a drawer the user has just opened. Explicit conversation
  navigation still closes it. A delayed-response browser regression covers both
  paths, alongside opening project actions from a phone-sized viewport.
- **Shared dialogs:** Tab and Shift+Tab remain inside the topmost dialog, with
  disabled, hidden and inert controls excluded. Existing Escape and focus-return
  behavior remains covered.

## Demos and contributor workflow

All three README GIFs are refreshed from the current UI. Web recordings use
scripted, fictional API responses and the TUI uses paced scripted SSE. They are
explicitly labeled examples, not evidence of live model or sandbox execution.
The web recorder starts its own app, asserts the views it records, and fails on
missing content. It no longer requires model credentials or silently accepts a
missing Upcoming tab. Concurrent browser suites and recordings use separate
throwaway authentication keys, so starting one cannot invalidate the other
run’s magic-link fixtures. See [the recording instructions](generating-demo-gif.md).

Contributor instructions now use the tagged Makefile test/vet path and the
complete web gate, including dependency audits and the override canary.

## Deliberate limits

This is a targeted code and behavior review, not a certification of the entire
repository or a penetration test. No feature was removed solely because it was
large or unfamiliar. Independent modal implementations were not consolidated.
The backend-specific Podman/Kubernetes execution paths remain mandatory; this
review does not introduce a host fallback or change their deployment contract.
Live sandbox and cluster integration still require their dedicated environments.
