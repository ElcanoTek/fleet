"use client";

// InjectedContextNote — the collapsed system note for a user turn's
// server-injected context (QA finding #6).
//
// Every chat turn used to have fleet's own additions concatenated onto the
// user's message text before the model saw it: the attachment manifest, the
// workspace inventory, the "Shared file library (files your administrator
// published…)" announcement, expanded `@file`/`@url` handles, the skill note,
// connector hints. Rendering that inside the user's bubble told the reader
// they had typed it — confusing for the person who sent it and alarming to a
// client reading a shared transcript ("why is my message full of text I
// didn't write?").
//
// Since migration 056 the suffix lives in its own column and arrives as
// `injected_context` (docs/ATTACHMENT-SCOPING.md, ADR-0058), so the transcript
// can put it where it belongs: attached to the turn, OUTSIDE the bubble, named
// for what it is, and collapsed — the reader is entitled to see exactly what
// the model was given, but it is not the thing they came to read.
//
// Presentation deliberately mirrors ReasoningBlock in ChatTranscript (the
// transcript's existing chrome for non-user-authored material): a bordered,
// tinted panel with a small uppercase muted label and a Show/Hide affordance
// on a real <button>, so it is keyboard-operable and exposes aria-expanded
// with no extra handlers.
//
// The body is rendered VERBATIM (pre-wrap, monospace) rather than through the
// markdown pipeline. Two reasons: the point of the panel is to show what the
// model actually received, byte for byte — a rendered `**Shared file
// library**` heading reads as prose someone wrote, which is the confusion this
// fix exists to remove — and a collapsed panel should not drag the lazy
// markdown chunk in behind it.

import { useId, useState } from "react";
import { Icon } from "./Icon";

export function InjectedContextNote({ text }: { text: string | undefined }) {
  const [expanded, setExpanded] = useState(false);
  const panelId = useId();

  // Nothing injected (an assistant turn, a legacy pre-056 row, or a plain
  // question with no attachments) renders no element at all — not an empty
  // panel. See messageHasRenderableContent in transcriptRows.ts for why an
  // empty container is never an acceptable stand-in here.
  if (!text || !text.trim()) return null;

  return (
    <div className="mt-1.5 w-full rounded-[var(--radius-lg)] border border-dashed border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_68%,transparent)] px-3 py-2 text-left text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]">
      <button
        type="button"
        className="flex w-full items-center justify-between gap-3 text-left"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        aria-controls={panelId}
      >
        <span className="flex min-w-0 items-center gap-1.5">
          <Icon name="info" className="size-3 shrink-0 text-[var(--color-text-muted)]" />
          {/* The accessible name says whose words these are NOT. "Context
              fleet added" is the whole point: a reader skimming a shared
              transcript must be able to tell at a glance that this text was
              not typed by the person whose bubble sits above it. */}
          <span className="min-w-0 truncate text-[0.68rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            Context fleet added — not typed by the sender
          </span>
        </span>
        <span className="shrink-0 text-[0.68rem] text-[var(--color-text-muted)]">
          {expanded ? "Hide" : "Show"}
        </span>
      </button>
      <div id={panelId} hidden={!expanded}>
        <p className="mt-2 text-[0.7rem] text-[var(--color-text-muted)]">
          fleet appended this to the message before the model read it —
          attached-file and workspace listings, the shared file library, and any
          expanded <code className="font-mono">@file</code> /{" "}
          <code className="font-mono">@url</code> references. Shown verbatim, as
          the model received it.
        </p>
        <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-[var(--color-border)] pt-2 font-mono text-[0.7rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {text.trim()}
        </pre>
      </div>
    </div>
  );
}
