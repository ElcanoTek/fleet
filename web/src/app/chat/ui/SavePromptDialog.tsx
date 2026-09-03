"use client";

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { Icon } from "@/app/shared/ui/Icon";
import {
  orchestratorApi,
  type PromptLibraryWrite,
} from "@/app/shared/lib/orchestratorApi";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";

// SavePromptDialog — "Save as workflow…". It asks the server to turn the
// WHOLE conversation into a reusable workflow template (a host-side model
// call — the same synthesis pattern as "Make recurring task…"), then shows
// the draft in this editable form so the user reviews and trims it BEFORE
// anything is saved. Saving goes through the orchestrator's existing
// POST /prompts, so permissions and the library list behave exactly as if the
// entry were authored by hand.
//
// What comes back is a procedure, not a question: objective, inputs as
// fillable placeholders, the numbered steps with the tools each used, the
// output shape, and the pitfalls. That is what makes a good chat worth
// keeping — a teammate can run it next quarter against different inputs.
// Saving one exchange, or one refined ask, would keep the answer and lose the
// method, so the synthesis always reads the entire conversation.

type Props = {
  conversationId: string;
  conversationTitle: string;
  onClose: () => void;
};

type Phase = "loading" | "edit" | "saved";

const FIELD =
  "w-full rounded-md border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]";

export function SavePromptDialog({
  conversationId,
  conversationTitle,
  onClose,
}: Props) {
  const [phase, setPhase] = useState<Phase>("loading");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [draft, setDraft] = useState<PromptLibraryWrite>({
    name: "",
    description: "",
    content: "",
    visibility: "private",
  });
  // Bumped by the Retry button to re-run the distillation effect.
  const [attempt, setAttempt] = useState(0);
  const modalRef = useRef<HTMLDivElement | null>(null);

  useDialogA11y(true, modalRef, onClose);

  useEffect(() => {
    // No synchronous setState here (strict react-hooks rule): the initial
    // state is already "loading", and the Retry button resets phase/error in
    // its click handler before bumping `attempt`.
    const controller = new AbortController();
    let cancelled = false;
    (async () => {
      try {
        const response = await fetch(
          `/api/conversations/${conversationId}/suggest-prompt`,
          { method: "POST", signal: controller.signal },
        );
        if (!response.ok) {
          throw new Error(
            (await response.text()) || `distill failed (${response.status})`,
          );
        }
        const body = (await response.json()) as {
          name?: string;
          description?: string;
          content?: string;
        };
        if (cancelled) return;
        setDraft({
          name: body.name ?? "",
          description: body.description ?? "",
          content: body.content ?? "",
          visibility: "private",
        });
        setPhase("edit");
      } catch (err) {
        if (cancelled || controller.signal.aborted) return;
        setError(
          err instanceof Error
            ? err.message
            : "Could not build a workflow from this conversation",
        );
        setPhase("edit");
      }
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [conversationId, attempt]);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      await orchestratorApi.createPrompt(draft);
      setPhase("saved");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save prompt");
    } finally {
      setSaving(false);
    }
  };

  // Distillation failed and there's nothing to edit yet → offer a retry
  // instead of an empty form.
  const distillFailed = phase === "edit" && error !== null && !draft.content;

  return createPortal(
    <div
      className="fixed inset-0 z-[1200] flex items-center justify-center bg-black/60 p-0 sm:p-4"
      role="dialog"
      aria-modal="true"
      aria-label="Save as workflow"
    >
      <div
        ref={modalRef}
        className="flex h-[100dvh] w-full flex-col overflow-hidden border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-2xl sm:h-auto sm:max-h-[min(44rem,92vh)] sm:w-[min(42rem,96vw)] sm:rounded-xl"
      >
        <header className="flex items-center gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-4 py-3">
          <span
            aria-hidden="true"
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
          >
            <Icon name="book" className="size-4.5" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="m-0 truncate text-base font-semibold text-[var(--color-text-primary)]">
              Save as workflow
            </h2>
            <p className="m-0 truncate text-xs text-[var(--color-text-muted)]">
              From “{conversationTitle}”
            </p>
          </div>
          <CloseButton label="Close save-workflow dialog" onClick={onClose} />
        </header>

        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
          {phase === "loading" ? (
            <div
              className="flex flex-col items-center gap-2 py-12 text-sm text-[var(--color-text-muted)]"
              data-testid="save-prompt-loading"
            >
              <span
                aria-hidden="true"
                className="size-5 animate-spin rounded-full border-2 border-[var(--color-border-strong)] border-t-[var(--color-accent)]"
              />
              Reading the whole chat and writing it up as a reusable
              workflow…
            </div>
          ) : phase === "saved" ? (
            <div
              className="flex flex-col items-center gap-3 py-12 text-center"
              data-testid="save-prompt-saved"
            >
              <span
                aria-hidden="true"
                className="flex h-10 w-10 items-center justify-center rounded-full bg-[var(--color-status-success-bg)] text-[var(--color-status-success-fg)]"
              >
                <Icon name="check" className="size-5" />
              </span>
              <p className="m-0 text-sm text-[var(--color-text-primary)]">
                Saved “{draft.name.trim() || "workflow"}” to your library.
              </p>
              <p className="m-0 text-xs text-[var(--color-text-muted)]">
                Find it under the book icon next to the message box.
              </p>
            </div>
          ) : distillFailed ? (
            <div className="flex flex-col items-center gap-3 py-12 text-center">
              <p className="m-0 max-w-96 text-sm text-[var(--color-status-error-fg)]">
                {error}
              </p>
              <button
                type="button"
                className="btn btn-secondary"
                onClick={() => {
                  setPhase("loading");
                  setError(null);
                  setAttempt((a) => a + 1);
                }}
              >
                Try again
              </button>
            </div>
          ) : (
            <div className="grid gap-3">
              <p className="m-0 text-xs text-[var(--color-text-muted)]">
                Written up from the whole conversation — the steps, the tools
                each one used, and what to watch out for. Fill-in points are
                marked in <code>[BRACKETS]</code>. Review and edit it before
                saving; nothing is stored until you do.
              </p>
              <label className="grid gap-1 text-sm">
                Name
                <input
                  className={FIELD}
                  maxLength={120}
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
              </label>
              <label className="grid gap-1 text-sm">
                Description{" "}
                <span className="text-xs text-[var(--color-text-muted)]">
                  Optional — helps teammates find it.
                </span>
                <input
                  className={FIELD}
                  maxLength={1024}
                  value={draft.description}
                  onChange={(e) =>
                    setDraft({ ...draft, description: e.target.value })
                  }
                />
              </label>
              <label className="grid gap-1 text-sm">
                Workflow
                <textarea
                  className={`${FIELD} min-h-80 font-mono text-xs`}
                  value={draft.content}
                  onChange={(e) =>
                    setDraft({ ...draft, content: e.target.value })
                  }
                />
              </label>
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  checked={draft.visibility === "workspace"}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      visibility: e.target.checked ? "workspace" : "private",
                    })
                  }
                />{" "}
                Share with this workspace
              </label>
              {error ? (
                <p className="m-0 text-sm text-[var(--color-status-error-fg)]">
                  {error}
                </p>
              ) : null}
            </div>
          )}
        </div>

        <footer className="flex justify-end gap-2 border-t border-[var(--color-border)] px-4 py-3">
          {phase === "saved" ? (
            <button type="button" className="btn btn-primary" onClick={onClose}>
              Done
            </button>
          ) : (
            <>
              <button
                type="button"
                className="btn btn-secondary"
                onClick={onClose}
              >
                Cancel
              </button>
              {distillFailed ? null : (
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={
                    phase !== "edit" ||
                    saving ||
                    !draft.name.trim() ||
                    !draft.content.trim()
                  }
                  onClick={() => void save()}
                >
                  {saving ? "Saving…" : "Save workflow"}
                </button>
              )}
            </>
          )}
        </footer>
      </div>
    </div>,
    document.body,
  );
}

export default SavePromptDialog;
