# Keyboard shortcuts

The chat shell ships a small, discoverable keyboard-shortcut layer (#306). Every
shortcut here has an equivalent mouse affordance — nothing is keyboard-only — and
the same list is shown in-app via the **?** help overlay (open it from the
sidebar's keyboard-shortcuts button or by pressing `?`).

Shortcuts marked **Mod** use `⌘` on macOS and `Ctrl` on Windows/Linux. The
shell never hijacks keys while you are typing: bare-letter shortcuts (`?`, `J`,
`K`, `P`, …) are suppressed inside text fields so they land as text, while
`Mod`-qualified shortcuts (e.g. `⌘K`) still work from inside the composer.

## Global

| Shortcut | Action |
| --- | --- |
| `Mod` + `K` | Open the search palette |
| `Mod` + `F` | Open the search palette (suppressed while typing, so the browser's in-page find still works there) |
| `Mod` + `N` | Start a new conversation |
| `Mod` + `J` | Focus the message composer |
| `?` | Show the keyboard-shortcut help overlay |
| `Esc` | Close the search palette, the help overlay, or the sidebar |

## Conversation list

Bare-key navigation over the conversation rail. `J`/`K` move a **focus cursor**
(a ring, distinct from the currently-open conversation) through the exact list
the sidebar shows — the filtered results when a filter is active, otherwise
Pinned-then-Recent. The remaining keys act on the focused row.

| Shortcut | Action |
| --- | --- |
| `J` | Focus the next conversation |
| `K` | Focus the previous conversation |
| `Enter` | Open the focused conversation |
| `P` | Pin / unpin the focused conversation |
| `A` | Archive the focused conversation |
| `R` | Rename the focused conversation (opens its inline editor) |
| `#` | Delete the focused conversation (opens the confirm dialog — never a silent delete) |

## Composer

| Shortcut | Action |
| --- | --- |
| `Enter` | Send the message |
| `Mod` + `Enter` | Send the message |
| `Shift` + `Enter` | Insert a newline |

## Current conversation

Act on the conversation you have open.

| Shortcut | Action |
| --- | --- |
| `Y` | Copy the last assistant response to the clipboard |
| `E` | Edit your last message (opens its inline editor) |

`Enter` on the conversation list is only active while a focus cursor is set, so
it never interferes with `Enter` elsewhere (e.g. activating a focused button).

## Not yet wired

The original proposal (#306) also sketched per-task **orchestrator** controls
and a full command palette that searches tasks/personas/slash-commands. Those
are intentionally **not** shipped yet: the `?` overlay lists only the shortcuts
that are actually wired, so it never advertises a key that does nothing. The
shared `useKeyboardShortcuts` hook is the seam to add the rest incrementally.
