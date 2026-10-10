"use client";

// Rows fleet writes itself, rendered as notices — never as the user's words.
//
// AutoContinueNotice is the input of a turn fleet started on its own after an
// approval card was settled (agent_policy.critical_tool_resume,
// docs/RESUME-AFTER-APPROVAL.md). It is stored as the turn's user-role entry
// (the model reads it as the turn's input), so without this it would render
// as a bubble the user never typed. The label says what happened; the exact
// text the model received sits behind Show, verbatim, like the injected
// context note.
//
// FleetNotices is the muted line(s) for notice entries that are not a turn —
// e.g. an automatic continue that was skipped (the hourly cap) or dropped by a
// restart.

import { useId, useState } from "react";
import { Icon } from "./Icon";

export function AutoContinueNotice({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const panelId = useId();
  return (
    <div
      data-testid="auto-continue-notice"
      className="w-full rounded-[var(--radius-lg)] border border-dashed border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_68%,transparent)] px-3 py-2 text-left text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]"
    >
      <button
        type="button"
        className="flex w-full items-center justify-between gap-3 text-left"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        aria-controls={panelId}
      >
        <span className="flex min-w-0 items-center gap-1.5">
          <Icon name="info" className="size-3 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 truncate text-[0.68rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            Continued automatically after an approval — not typed by you
          </span>
        </span>
        <span className="shrink-0 text-[0.68rem] text-[var(--color-text-muted)]">
          {expanded ? "Hide" : "Show"}
        </span>
      </button>
      <div id={panelId} hidden={!expanded}>
        <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-[var(--color-border)] pt-2 font-mono text-[0.7rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {text.trim()}
        </pre>
      </div>
    </div>
  );
}

export function FleetNotices({ notices }: { notices: string[] | undefined }) {
  if (!notices || notices.length === 0) return null;
  return (
    <div className="grid gap-1" data-testid="fleet-notices">
      {notices.map((text, i) => (
        <p
          key={i}
          className="flex items-start gap-1.5 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]"
        >
          <Icon name="info" className="mt-0.5 size-3 shrink-0" />
          <span className="min-w-0">{text}</span>
        </p>
      ))}
    </div>
  );
}
