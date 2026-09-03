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

// SavePromptDialog — "Save to prompt library…" from a conversation's kebab
// menu. It asks the server to distill the conversation into a reusable
// prompt-library draft (a host-side model call — the same synthesis pattern
// as "Make recurring task…"), then shows the draft in this editable form so
// the user reviews and trims it BEFORE anything is saved. Saving goes through
// the orchestrator's existing POST /prompts, so permissions and the library
// list behave exactly as if the prompt were authored by hand.

type Props = {
  conversationId: string;
  conversationTitle: string;
  // Set when the user saved from a specific assistant reply ("Save as prompt"
  // under a message): the server cuts the distilled transcript there, so a
  // later tangent in the same chat can't leak into the saved recipe. Omitted
  // by the conversation-level menu item, which means "the whole chat".
  upToMessageId?: number;
  onClose: () => void;
};

type Phase = "loading" | "edit" | "saved";

const FIELD =
  "w-full rounded-md border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 text-sm text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]";

export function SavePromptDialog({
  conversationId,
  conversationTitle,
  upToMessageId,
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
          {
            method: "POST",
            signal: controller.signal,
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(
              upToMessageId ? { up_to_message_id: upToMessageId } : {},
            ),
          },
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
            : "Could not distill a prompt from this conversation",
        );
        setPhase("edit");
      }
    })();
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [conversationId, upToMessageId, attempt]);

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
      aria-label="Save to prompt library"
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
              Save to prompt library
            </h2>
            <p className="m-0 truncate text-xs text-[var(--color-text-muted)]">
              From “{conversationTitle}”
            </p>
          </div>
          <CloseButton label="Close save-prompt dialog" onClick={onClose} />
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
              Distilling a reusable prompt from this conversation…
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
                Saved “{draft.name.trim() || "prompt"}” to your library.
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
                {upToMessageId
                  ? "This draft was distilled from the chat up to the reply you saved from — review and trim it before saving. Nothing is stored until you save."
                  : "This draft was distilled from the conversation — review and trim it before saving. Nothing is stored until you save."}
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
                Prompt
                <textarea
                  className={`${FIELD} min-h-56 font-mono text-xs`}
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
                  {saving ? "Saving…" : "Save prompt"}
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
