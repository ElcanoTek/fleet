# Remembered email recipients in the task form

The task form's **Email results** field used to start empty every time, so a
client distribution list had to be retyped for each new task. The browser's
own autofill offered personal addresses over it. moc, the old Operations
Center, kept every added address in the browser's `localStorage`. fleet
derives the list from the tasks themselves, so it works on any device and is
already full the first time someone opens the form.

## What shipped

- **`GET /tasks/recipients`** (`handlers.GetRecipientHistory`,
  `db.GetRecipientHistory`). Recipients are not a column. They live in the
  CRITICAL ACTION block that `web/src/app/orchestrator/taskEmailBlock.ts`
  appends to the prompt, one `    - address` line each. The query looks for
  the block marker and reads address lines only from there on, so a list the
  author wrote in the prompt body is never matched. It returns each distinct
  lower-cased address with:
  - `task_count`: distinct lineages, so a recurring task with a hundred runs
    counts once;
  - `last_used`.
  It covers tasks created in the last 180 days, most recent first, capped at
  200. The handler drops anything `net/mail` does not parse as a bare address.
- **Visibility** is the tag catalogue's (#1082). An admin, or a holder of the
  fleet-wide grant, sees every task's recipients. A non-admin user or scoped
  API key sees only addresses on tasks it created.
- **The field is a combobox** over those addresses, ranked in
  `recipientSuggestions.ts`.
  - On focus it lists up to 8 suggestions. With nothing typed they are ordered
    most used first. Typing narrows them, with matches at the start of the
    address or of a word (`blu`, `weather`) ahead of mid-word matches.
  - Addresses already added are left out.
  - Arrow keys, Enter, click and Escape work as in ModelPicker. Browser
    autofill is off on the input.
  - A failed fetch leaves plain typing unchanged.
- **Escape inside a dialog.** `useDialogA11y` listens for Escape in the
  capture phase, so a combobox could not keep its own Escape: the first press
  closed the whole form. The hook now leaves Escape to a focused
  `role="combobox"` with `aria-expanded="true"`. The list closes, and the next
  Escape reaches the dialog. This also fixes the same trap for ModelPicker in
  the task form.

## Deviations and limits

- There is no per-person "forget this address", unlike moc's `localStorage`
  chips. An address leaves the list when no visible task created in the last
  180 days uses it.
- Suggestions come from tasks only. Addresses typed into a form that was
  never saved are not remembered.
- The query scans prompts of tasks in the window. On Elcano's production data
  (972 tasks, 212 with recipients) it ran in about 105 ms.
