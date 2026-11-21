import { t as tr } from "@/app/lib/i18n";
// The note an administrator pinned to this chat (ADR-0117). It sits above the
// conversation for the chat's people until the administrator removes it.
//
// It is not a message: it is no part of the chat's history, so the assistant
// never reads it and nothing in the conversation refers to it. The banner
// says so, because a person who replies to it in the composer is talking to
// the assistant, not to the administrator.
//
// "Got it" tells the administrator the note was read and folds the banner to
// one line; it stays pinned, and opens again on a click, until it is removed.

import { useId, useState } from "react";
import { Icon } from "./Icon";

export type AdminNoteState = {
  text: string;
  author: string;
  updated_at: number;
  /** Names this text; "Got it" sends it back so a replaced note stays unread. */
  version: number;
  seen_at?: number;
  seen_by?: string;
};

export function AdminNoteBanner({
  note,
  onAcknowledge,
}: {
  note: AdminNoteState;
  // Records that the note was read; resolves false when that failed.
  onAcknowledge: () => Promise<boolean>;
}) {
  const seen = Boolean(note.seen_at);
  // Open until acknowledged; afterwards folded unless the reader opens it.
  const [opened, setOpened] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const bodyId = useId();
  const expanded = !seen || opened;
  const frame =
    "rounded-[0.625rem] border border-[color-mix(in_srgb,var(--color-accent)_36%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_7%,var(--color-surface-1))] text-[0.8rem] text-[var(--color-text-secondary)]";
  const title = note.author ? tr("Note from an administrator · {0}", [note.author]) : tr("Note from an administrator");
  if (!expanded) {
    return (
      <button
        type="button"
        data-testid="admin-note-banner"
        aria-expanded={false}
        aria-controls={bodyId}
        onClick={() => setOpened(true)}
        className={`${frame} flex w-full items-center gap-2 px-3 py-1.5 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11`}
      >
        <Icon name="pin" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
        <span className="shrink-0 font-medium text-[var(--color-text-primary)]">{tr("Note from an administrator")}</span>
        <span className="min-w-0 flex-1 truncate text-[var(--color-text-muted)]">{note.text}</span>
        <span className="shrink-0 text-[0.72rem] text-[var(--color-accent)]">{tr("Show")}</span>
      </button>
    );
  }
  return (
    <div role="note" aria-label={tr("Note from an administrator")} data-testid="admin-note-banner" className={`${frame} px-3.5 py-3`}>
      <div className="flex items-center gap-2 text-[0.7rem] font-semibold uppercase tracking-[0.07em] text-[var(--color-text-secondary)]">
        <Icon name="pin" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
        <span className="min-w-0 truncate">{title}</span>
      </div>
      <p id={bodyId} className="mb-0 mt-1.5 whitespace-pre-wrap text-[0.875rem] leading-[1.55] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
        {note.text}
      </p>
      <div className="mt-2.5 flex flex-wrap items-center justify-between gap-x-3 gap-y-1.5">
        <span className="min-w-0 flex-1 basis-56 text-[0.72rem] leading-[1.5] text-[var(--color-text-muted)]">
          {failed
            ? tr("Couldn’t record that. Try again.")
            : tr("Only people in this chat see this note. The assistant doesn’t read it, so replying here won’t reach the administrator.")}
        </span>
        {seen ? (
          <button
            type="button"
            className="rounded-[var(--radius-md)] px-2 py-1 text-[0.75rem] font-medium text-[var(--color-accent)] hover:underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
            onClick={() => setOpened(false)}
          >
            {tr("Hide")}
          </button>
        ) : (
          <button
            type="button"
            data-testid="admin-note-ack"
            disabled={busy}
            className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-2.5 py-1 font-medium whitespace-nowrap text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60 any-pointer-coarse:min-h-11"
            onClick={() => {
              setBusy(true);
              setFailed(false);
              void onAcknowledge()
                .then((ok) => setFailed(!ok))
                .finally(() => setBusy(false));
            }}
          >
            <Icon name="check" className="size-3.5 shrink-0" />
            {tr("Got it")}
          </button>
        )}
      </div>
    </div>
  );
}
