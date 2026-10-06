"use client";

import { useEffect, useId, useRef, useState } from "react";
import { Icon } from "./Icon";
import { TeamGlyph } from "./ShareGlyphs";

// The project home's header pill (Fleet Projects sharing, B6/B7, decision
// #17): who can see THIS PROJECT, in the same two words the rest of the
// sharing UI uses — "Only you" or "Shared with <team>". The shared pill opens
// a small popover explaining what the team actually sees, because "shared"
// alone read as "every chat in here is visible", which it never was: sharing
// the project shares its definition, and each chat stays Only you until its
// owner shares it (ADR-0057).
//
// The popover copy is the SPEC's, not the prototype's: B7 still says "Files
// are shared one at a time", which stopped being true when sharing a chat
// began carrying its outputs.

export const pillBase =
  "inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 text-[0.7rem]";

export function ProjectVisibilityPill({
  teamName,
  isOwner,
  onOpenSettings,
}: {
  // The team the project is shared with; "" / undefined = a personal project.
  teamName?: string;
  isOwner: boolean;
  onOpenSettings: () => void;
}) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const popoverId = useId();

  // Click-away and Escape close it — a disclosure, not a modal, so it does
  // not trap focus or dim the page.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (!teamName) {
    return (
      <span
        data-testid="project-visibility-pill"
        className={`${pillBase} border-[var(--color-border)] text-[var(--color-text-secondary)]`}
      >
        <Icon name="lock" className="size-3" />
        Only you
      </span>
    );
  }

  return (
    <div ref={wrapRef} className="relative shrink-0">
      <button
        type="button"
        data-testid="project-visibility-pill"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? popoverId : undefined}
        title={`Shared with ${teamName}`}
        className={`${pillBase} border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-text-primary)] transition hover:bg-[color-mix(in_srgb,var(--color-primary)_22%,transparent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]`}
        onClick={() => setOpen((o) => !o)}
      >
        <TeamGlyph className="size-3" />
        Shared with {teamName}
        <Icon name="chevron-down" className="size-3" />
      </button>
      {open ? (
        <div
          id={popoverId}
          role="dialog"
          aria-label="Who sees this project"
          className="absolute left-0 top-full z-40 mt-1.5 w-[19rem] max-w-[calc(100vw-2rem)] rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-3.5 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)] shadow-[var(--shadow-md)]"
        >
          <p className="m-0 mb-2 text-[0.82rem] font-semibold text-[var(--color-text-primary)]">
            Shared with {teamName}
          </p>
          <p className="m-0 mb-1.5 flex gap-2">
            <TeamGlyph className="mt-[0.2rem] size-3.5 shrink-0 text-[var(--color-accent)]" />
            <span>
              {teamName} sees the instructions, Team learnings, and any chat
              shared with them.
            </span>
          </p>
          <p className="m-0 mb-1.5 flex gap-2">
            <Icon
              name="lock"
              className="mt-[0.2rem] size-3.5 shrink-0 text-[var(--color-accent)]"
            />
            <span>
              Each chat stays Only you until its owner shares it. Its files go
              with it.
            </span>
          </p>
          <p className="m-0 flex gap-2">
            <Icon
              name="info"
              className="mt-[0.2rem] size-3.5 shrink-0 text-[var(--color-accent)]"
            />
            <span>
              Chats here use the instructions, then Team learnings, then each
              person&rsquo;s own memory. They don&rsquo;t expire.
            </span>
          </p>
          {isOwner ? (
            <button
              type="button"
              className="mt-2.5 rounded-[var(--radius-md)] text-[0.78rem] font-medium text-[var(--color-accent)] underline-offset-2 hover:underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={() => {
                setOpen(false);
                onOpenSettings();
              }}
            >
              Project settings
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
