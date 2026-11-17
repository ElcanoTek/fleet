# Web UI rules from the November 2026 UI audit

A batch of fixes to the web UI, found by auditing every page at phone, tablet
and desktop widths, in light and dark, in English, Spanish and Portuguese, as
an admin and as a member. This note records the rules those fixes left behind,
because several of them replace a guess with a rule and the rule is the part
worth keeping. Per-screen fixes are in the pull request, not repeated here.

## What shipped

### Rules for content a model or a person wrote

- **A literal `<br>`** is a line break only inside a table cell, and only when
  it has content before and after it in that cell. Everywhere else it is shown
  as typed, like any other raw HTML. Outside a table the same characters mean
  either "break here" or "the tag I am talking about", and nothing in the text
  settles which; three attempts to guess each hid something a person had
  typed. One plugin list (`web/src/app/shared/lib/markdownPlugins.ts`) serves
  the chat, run logs and generated cards, so the same text reads the same in
  all three. Cases are pinned as tables in
  `chat-experience.markdown.test.tsx`.
- **Words longer than their line** may break (`overflow-wrap: break-word`,
  inherited from `html`). Text that fits is laid out exactly as before.
  `overflow-wrap: anywhere` is for values (paths, ids, names) in a box that
  must be able to shrink below its longest word; it is wrong for a label made
  of ordinary words, because it lets the box become narrower than a word and
  splits it. `layoutDefaults.test.ts` keeps it off the stat-card titles.

### Layout defaults for long values

Three defaults in `@layer base` of `web/src/app/globals.css`, so they lose to
anything more specific:

1. Tailwind's bare `grid` gets `grid-template-columns: minmax(0, 1fr)`: one
   column that may be narrower than its content. Grids that flow in columns
   or position their column (`place-content-*`, `justify-center`, …) are left
   alone. Every `display: grid` rule in `globals.css` declares its columns.
2. A badge (`[data-slot="badge"]`) is at most as wide as its container. A
   badge that shows a value from data wraps it in `<span class="truncate">`.
3. The word-break default above.

They do not cover a flex row: there the item that holds the value still needs
`min-w-0` plus `truncate` or a wrap. A label next to a fixed-size chip that
must never split a word uses `min-w-min flex-[1_1_0]` in a wrapping row, so
the chip drops to its own line instead (Connections, category tiles).

### Forms

- `FormActions` (`web/src/app/settings/ui/atoms.tsx`) is the footer of a
  settings form: the buttons are one group that stays on one line; the
  `MissingFieldsHint` sits beside them when the footer fits on a line and
  takes the line above when it does not. A form whose button follows its
  fields in one wrapping row uses the hint's `block` form: a full line under
  the button.
- A greyed-out submit button says which fields are missing
  (`aria-describedby` points at the hint). The `missing` list must be exactly
  the condition that disables the button.
- `useFocusOnOpen(open, item)` puts the cursor in the first field when a form
  opens and when the open form is given another row to edit.

### Keyboard and focus

- The phone sidebar (below `sm`) takes focus when it opens, keeps Tab inside
  and closes on Escape. The one exception: opened by the `j` / `k` shortcut
  while nothing that owns Enter has focus, focus stays where it is so Enter
  opens the chat under the list cursor. The sidebar and that Enter use one
  test (`web/src/app/shared/ui/interactiveTarget.ts`).
- Escape closes the top layer only: a menu or dialog over the sidebar closes
  by itself.

### Following the end of a chat

`web/src/app/chat/ui/stickToBottom.ts`:

- "The reader is at the end" is kept from scroll events (`trackPin`), measured
  from where they were last level with the end, not from the previous event,
  so a slow scroll up in sub-pixel steps counts.
- A reader at the end is carried to new content however far it moved. A
  reader who scrolled up further than the follow window (240px while a reply
  streams, 160px at rest) is not pulled back. A row that only changes height
  at rest is followed only for a reader who was at the end.
- While our own smooth scroll is under way, distance is judged when it lands.
- A hidden transcript (the Dashboard's Board pane, a page laid over the chat)
  takes no decision, and the reader is put back where they were when it shows
  again. The app placing the reader (the Board's jump to a message) overrides
  that (`requestHoldPosition`); a send or Jump to latest asks to be taken to
  the end (`requestFollowBottom`).

### Guard tests

- `web/src/app/layoutDefaults.test.ts`: the three defaults exist, no grid in
  `globals.css` goes without declared columns, label titles do not break
  inside a word.
- `web/e2e/mocked/layout-long-values.spec.ts`: System checks, Prompts and the
  chat sidebar with 100-character unbroken values at 320, 390, 768 and
  1280px; nothing past the screen, no sideways scroll, no button pushed out
  of view.

## Deviations

- Two `@flag/*` registry components were edited in place
  (`web/src/components/ui/switch.tsx`: a larger touch target;
  `web/src/components/ui/tabs.tsx`: a focus ring that renders). They are not
  in `flag.lock`, so a later `npx shadcn add` would overwrite them; both
  should go upstream to Flag.
- Dashboard view between 640 and 767px no longer shows the mark beside the
  title or the Esc keycap on Exit: that is the room the status needs to stay
  whole in Spanish and Portuguese. Escape still exits.
- Below 375px the six Background-tasks stat cards are two across rather than
  three, so no title has to split a word.

## What it does not do

- `<br>` outside a table is not a line break, so an address written with the
  tag at the end of each line shows the tag (as it did before this work).
  While a table streams in, a tag at the end of the cell being written shows
  until the next piece of text arrives.
- The Patrol Launchpad's Launch button still gives no reason when it is
  greyed out.
- Sending while a reply is still running queues the message and does not take
  the reader to the end.
- Inside the follow window a new message or streamed text still carries the
  reader to the end even if they had scrolled up a little: while a reply is
  streaming, one mouse-wheel notch up (about 100px) is taken back by the next
  piece of text, and only a larger scroll holds. The windows are unchanged
  from before this work; now that "the reader scrolled up" is tracked
  reliably, they could be dropped for such a reader.
- Under reduced motion, a last row that grows by more than the idle window
  after a reply finishes is not followed (the same as before).
- At 320px in Spanish and Portuguese, Background tasks, Prompts and Features
  still scroll sideways by 12–43px. That predates this work.
- The long-value defaults were tested with fields a person can type and with
  status, label and path fields. A value in a flex row without `min-w-0` can
  still widen its container; two such places are known and need
  system-defined values to trigger (a connection category, a memory kind).
