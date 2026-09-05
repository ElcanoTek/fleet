"use client";

import { useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { Icon } from "@/app/shared/ui/Icon";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";

// DownloadChatDialog — "Download chat…" from a conversation's kebab menu.
//
// This replaced a single menu item labelled "Download as JSON". That label
// asked the reader to know what JSON is, and then handed them a file most
// people cannot open usefully: the one format on offer was the archival one.
// The formats below are named for what the user gets to DO with the file, and
// the default is the one a non-technical person actually wants — a page that
// opens on a double-click and prints to PDF.
//
// The dialog is presentational: the caller owns the fetch + save, so the
// download path stays in one place (chat-experience) for every entry point.
//
// Not on the shared DialogShell (the B-2 pass), deliberately: this is the
// full-bleed "sheet" shape, not a centred panel — it portals to <body>, fills
// the viewport below sm:, carries its own header bar and a backdrop that is
// not click-to-dismiss, and traps Tab through useDialogA11y. Its panel is
// already opaque (--color-surface-1), which is the defect that pass fixed, so
// moving it onto the shell would only buy props that switch the shell's
// defaults back off. PromptLibrary, DownloadChatDialog and SavePromptDialog
// share this shape with each other.

export type DownloadFormat = "html" | "markdown" | "json";

export type DownloadOptions = {
  format: DownloadFormat;
  // Include the agent's working trail (tool calls, results, thinking). Ignored
  // for JSON, which is the archival shape and always carries everything.
  includeWork: boolean;
};

type Props = {
  conversationTitle: string;
  onDownload: (options: DownloadOptions) => Promise<void>;
  onClose: () => void;
};

const FORMATS: {
  id: DownloadFormat;
  icon: string;
  label: string;
  hint: string;
}[] = [
  {
    id: "html",
    icon: "globe",
    label: "Web page",
    hint: "Opens in any browser, looks like the chat. Print it to save a PDF.",
  },
  {
    id: "markdown",
    icon: "file-text",
    label: "Text document",
    hint: "Plain text you can paste into email, Word, Docs, or Notion.",
  },
  {
    id: "json",
    icon: "database",
    label: "Raw data",
    hint: "Every message, tool call and result exactly as stored. For developers.",
  },
];

export function DownloadChatDialog({
  conversationTitle,
  onDownload,
  onClose,
}: Props) {
  const [format, setFormat] = useState<DownloadFormat>("html");
  const [includeWork, setIncludeWork] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const modalRef = useRef<HTMLDivElement | null>(null);

  useDialogA11y(true, modalRef, onClose);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      await onDownload({ format, includeWork });
      // The browser's own Save dialog takes over from here; keeping ours open
      // behind it would just be a box to dismiss.
      onClose();
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Could not download this chat",
      );
      setBusy(false);
    }
  };

  return createPortal(
    <div
      className="fixed inset-0 z-[1200] flex items-center justify-center bg-black/60 p-0 sm:p-4"
      role="dialog"
      aria-modal="true"
      aria-label="Download chat"
    >
      <div
        ref={modalRef}
        className="flex h-[100dvh] w-full flex-col overflow-hidden border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-2xl sm:h-auto sm:max-h-[min(44rem,92vh)] sm:w-[min(34rem,96vw)] sm:rounded-xl"
      >
        <header className="flex items-center gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-4 py-3">
          <span
            aria-hidden="true"
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
          >
            <Icon name="download" className="size-4.5" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="m-0 truncate text-base font-semibold text-[var(--color-text-primary)]">
              Download chat
            </h2>
            <p className="m-0 truncate text-xs text-[var(--color-text-muted)]">
              “{conversationTitle}”
            </p>
          </div>
          <CloseButton label="Close download dialog" onClick={onClose} />
        </header>

        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
          <fieldset className="m-0 grid gap-2 border-0 p-0">
            <legend className="sr-only">File format</legend>
            {FORMATS.map((option) => {
              const selected = format === option.id;
              return (
                <label
                  key={option.id}
                  className={[
                    "flex cursor-pointer items-start gap-3 rounded-lg border p-3 transition",
                    selected
                      ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_8%,transparent)]"
                      : "border-[var(--color-border)] hover:border-[var(--color-border-strong)]",
                  ].join(" ")}
                >
                  <input
                    type="radio"
                    name="download-format"
                    className="mt-1"
                    value={option.id}
                    checked={selected}
                    onChange={() => setFormat(option.id)}
                  />
                  <span
                    aria-hidden="true"
                    className={
                      selected
                        ? "mt-0.5 text-[var(--color-accent)]"
                        : "mt-0.5 text-[var(--color-text-muted)]"
                    }
                  >
                    <Icon name={option.icon} className="size-4" />
                  </span>
                  <span className="min-w-0 grid gap-0.5">
                    <span className="text-sm font-medium text-[var(--color-text-primary)]">
                      {option.label}
                    </span>
                    <span className="text-xs text-[var(--color-text-muted)]">
                      {option.hint}
                    </span>
                  </span>
                </label>
              );
            })}
          </fieldset>

          {/* Raw data always carries the whole run, so the choice would be a
              lie there — hide it rather than show a dead control. */}
          {format === "json" ? null : (
            <label className="mt-4 flex items-start gap-2 text-sm text-[var(--color-text-primary)]">
              <input
                type="checkbox"
                className="mt-1"
                checked={includeWork}
                onChange={(e) => setIncludeWork(e.target.checked)}
              />
              <span className="grid gap-0.5">
                Include the agent&apos;s work
                <span className="text-xs text-[var(--color-text-muted)]">
                  Tool calls, their results, and the agent&apos;s thinking. Off
                  by default so the file reads like a conversation.
                </span>
              </span>
            </label>
          )}

          {error ? (
            /* role="alert" so a failed download is ANNOUNCED, matching the
               sibling dialogs (ShareDialog, shared-files): without it a screen
               reader user gets no signal at all that the export failed. */
            <p
              role="alert"
              className="mt-3 mb-0 text-sm text-[var(--color-status-error-fg)]"
            >
              {error}
            </p>
          ) : null}
        </div>

        <footer className="flex justify-end gap-2 border-t border-[var(--color-border)] px-4 py-3">
          <button type="button" className="btn btn-secondary" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={busy}
            onClick={() => void start()}
          >
            {busy ? "Preparing…" : "Download"}
          </button>
        </footer>
      </div>
    </div>,
    document.body,
  );
}

export default DownloadChatDialog;
