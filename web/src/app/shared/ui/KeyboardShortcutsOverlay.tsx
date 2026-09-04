"use client";

import { useMemo, useRef, useState } from "react";
import { isMacPlatform } from "@/app/shared/hooks/useKeyboardShortcuts";
import { CloseButton } from "./CloseButton";
import { DialogShell } from "./DialogShell";

// KeyboardShortcutsOverlay — the discoverable "?" help modal (#306). It lists
// every keyboard shortcut the shell binds, grouped by area, with platform-aware
// key chips (⌘ on macOS, Ctrl elsewhere). Type-to-filter narrows the list
// inline. The overlay chrome — opaque panel, click-outside scrim, Escape,
// focus-on-open — is the shared DialogShell's; the parent mounts this only
// while open, so each open is a fresh mount.
//
// Honesty note: this overlay documents only the shortcuts the shell actually
// wires. The catalog below is the single source of truth — adding a row here
// without wiring its handler would advertise a key that does nothing, so keep
// the two in step.

export type ShortcutChip =
  // A literal label rendered verbatim (e.g. "Esc", "Enter", "?").
  | { label: string }
  // The platform modifier — renders ⌘ on macOS, "Ctrl" elsewhere.
  | { mod: true }
  // The Shift modifier.
  | { shift: true };

export interface ShortcutHelpEntry {
  /** Ordered chips forming the key combo, e.g. [{mod:true},{label:"K"}]. */
  chips: ShortcutChip[];
  /** Human-readable description of what the shortcut does. */
  description: string;
}

export interface ShortcutHelpGroup {
  title: string;
  entries: ShortcutHelpEntry[];
}

function chipLabel(chip: ShortcutChip, mac: boolean): string {
  if ("mod" in chip) return mac ? "⌘" : "Ctrl";
  if ("shift" in chip) return mac ? "⇧" : "Shift";
  return chip.label;
}

// An accessible spoken form for the whole combo, e.g. "Command K" / "Control K".
function comboAriaLabel(chips: ShortcutChip[], mac: boolean): string {
  return chips
    .map((chip) => {
      if ("mod" in chip) return mac ? "Command" : "Control";
      if ("shift" in chip) return "Shift";
      return chip.label;
    })
    .join(" ");
}

export function KeyboardShortcutsOverlay({
  groups,
  onClose,
}: {
  groups: ShortcutHelpGroup[];
  onClose: () => void;
}) {
  const mac = isMacPlatform();
  const [filter, setFilter] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  // Focus-on-open (the filter input, so type-to-filter works immediately) and
  // close-on-Escape both come from DialogShell now — see that file. The panel
  // is also opaque there: over a dense transcript this overlay's rows used to
  // compete with the chat showing through it.

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return groups;
    return groups
      .map((group) => ({
        ...group,
        entries: group.entries.filter((entry) => {
          const haystack = `${group.title} ${entry.description} ${entry.chips
            .map((c) => chipLabel(c, mac))
            .join(" ")}`.toLowerCase();
          return haystack.includes(q);
        }),
      }))
      .filter((group) => group.entries.length > 0);
  }, [filter, groups, mac]);

  return (
    <DialogShell
      label="Keyboard shortcuts"
      scrimLabel="Close keyboard shortcuts"
      onDismiss={onClose}
      align="top"
      className="flex max-h-[80vh] max-w-[34rem] flex-col overflow-hidden"
      initialFocusRef={inputRef}
      testId="shortcuts-overlay"
    >
      <div className="flex items-center justify-between gap-3 border-b border-[var(--color-border-strong)] px-4 py-3">
        <h2 className="text-[0.95rem] font-semibold text-[var(--color-text-primary)]">
          Keyboard shortcuts
        </h2>
        <CloseButton
          label="Close keyboard shortcuts"
          testId="shortcuts-close"
          onClick={onClose}
        />
      </div>
      <input
        ref={inputRef}
        data-testid="shortcuts-filter"
        type="text"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder="Filter shortcuts…"
        className="w-full border-b border-[var(--color-border-strong)] bg-transparent px-4 py-3 text-[0.9rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)]"
        aria-label="Filter shortcuts"
      />
      <div className="max-h-[55vh] overflow-y-auto px-4 py-2" data-testid="shortcuts-list">
        {filtered.length === 0 ? (
          <div
            data-testid="shortcuts-empty"
            className="px-1 py-3 text-[0.85rem] text-[var(--color-text-muted)]"
          >
            No matching shortcuts.
          </div>
        ) : (
          filtered.map((group) => (
            <section key={group.title} className="py-2">
              <h3 className="mb-1.5 text-[0.7rem] font-semibold uppercase tracking-wide text-[var(--color-text-muted)]">
                {group.title}
              </h3>
              <ul className="flex flex-col gap-1">
                {group.entries.map((entry) => (
                  // Key by description + combo: two entries can share a
                  // description (e.g. Enter and ⌘Enter both "Send the
                  // message"), so the chips disambiguate and prevent a React
                  // key collision that would otherwise reuse a stale row when
                  // the inline filter re-renders the list.
                  <li
                    key={`${entry.description}|${entry.chips
                      .map((c) => chipLabel(c, mac))
                      .join("+")}`}
                    data-testid="shortcut-row"
                    className="flex items-center justify-between gap-4 py-1"
                  >
                    <span className="text-[0.85rem] text-[var(--color-text-secondary)]">
                      {entry.description}
                    </span>
                    <span
                      className="flex shrink-0 items-center gap-1"
                      aria-label={comboAriaLabel(entry.chips, mac)}
                    >
                      {entry.chips.map((chip, idx) => (
                        <kbd
                          key={idx}
                          aria-hidden="true"
                          className="inline-flex min-w-[1.5rem] items-center justify-center rounded-md border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-1.5 py-0.5 text-[0.72rem] font-medium text-[var(--color-text-primary)]"
                        >
                          {chipLabel(chip, mac)}
                        </kbd>
                      ))}
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          ))
        )}
      </div>
    </DialogShell>
  );
}
