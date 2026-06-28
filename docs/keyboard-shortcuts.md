# Keyboard shortcuts

The chat shell ships a small, discoverable keyboard-shortcut layer (#306). Every
shortcut here has an equivalent mouse affordance — nothing is keyboard-only — and
the same list is shown in-app via the **?** help overlay (open it from the
sidebar's keyboard-shortcuts button or by pressing `?`).

Shortcuts marked **Mod** use `⌘` on macOS and `Ctrl` on Windows/Linux. The
shell never hijacks keys while you are typing: bare-letter shortcuts (`?`) are
suppressed inside text fields so they land as text, while `Mod`-qualified
shortcuts (e.g. `⌘K`) still work from inside the composer.

## Global

| Shortcut | Action |
| --- | --- |
| `Mod` + `K` | Open the search palette |
| `Mod` + `F` | Open the search palette (suppressed while typing, so the browser's in-page find still works there) |
| `Mod` + `N` | Start a new conversation |
| `Mod` + `J` | Focus the message composer |
| `?` | Show the keyboard-shortcut help overlay |
| `Esc` | Close the search palette, the help overlay, or the sidebar |

## Composer

| Shortcut | Action |
| --- | --- |
| `Enter` | Send the message |
| `Mod` + `Enter` | Send the message |
| `Shift` + `Enter` | Insert a newline |

## Not yet wired

The original proposal (#306) sketched a larger set — `J`/`K` list navigation,
per-conversation actions (pin/archive/delete/rename), per-task orchestrator
controls, edit-last-message, copy-last-response, and a full command palette that
also searches tasks/personas/slash-commands. Those are intentionally **not**
shipped here: the `?` overlay lists only the shortcuts that are actually wired,
so it never advertises a key that does nothing. The shared
`useKeyboardShortcuts` hook is the seam to add the rest incrementally.
