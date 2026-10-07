"use client";

import { DialogShell } from "@/app/shared/ui/DialogShell";
import { deleteChatsText, useImpact } from "./ProjectSettingsDialog";

// The RAIL kebab's "Delete project" confirmation — B29's copy and counts, the
// same as the project settings dialog's inline delete panel (decision #50):
// chats are not deleted, they leave the project and become temporary; team
// learnings die with it; instructions and sharing go. The counts are real,
// from GET /api/projects/{id}/impact; until they land (or if the read fails)
// the copy states WHAT is lost without inventing how much. Export first is
// offered here too, so the rail path is not the uninformed one.
export function DeleteProjectConfirmDialog({
  projectId,
  projectName,
  onCancel,
  onConfirm,
}: {
  projectId?: string;
  projectName?: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  // No id (an older caller) = nothing to count: the copy states WHAT is lost
  // without inventing how much, and there is no export link.
  const fetched = useImpact(projectId ?? "", Boolean(projectId));
  const state = projectId ? fetched : { settled: true, impact: null };

  const name = projectName || "this project";
  return (
    <DialogShell
      label={`Delete ${name}?`}
      scrimLabel="Cancel deleting the project"
      onDismiss={onCancel}
      className="max-w-[28rem] p-5"
      testId="rail-delete-project-confirm"
    >
      <h2 className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        Delete {name}?
      </h2>
      <ul className="mb-4 grid list-disc gap-1 pl-[1.1rem] text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
        <li>
          {state.settled
            ? deleteChatsText(state.impact)
            : "Counting the chats in it…"}
        </li>
        <li>
          {state.impact
            ? `Team learnings (${state.impact.memories}) are lost.`
            : "Its team learnings are lost."}
        </li>
        <li>Instructions and sharing are removed.</li>
      </ul>
      <div className="flex flex-wrap items-center gap-2">
        {projectId ? (
          <a
            href={`/api/projects/${encodeURIComponent(projectId)}/export`}
            className="rounded-md border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Export first
          </a>
        ) : null}
        <span className="flex-1" />
        <button
          type="button"
          className="rounded-md px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          onClick={onCancel}
        >
          Cancel
        </button>
        <button
          type="button"
          // Waits for the counts the list promises; a failed read still
          // settles, with the honest "what, not how much" wording.
          disabled={!state.settled}
          className="rounded-md px-3 py-1.5 text-[0.8rem] font-medium text-[var(--color-danger)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] disabled:opacity-50"
          onClick={() => {
            if (state.settled) onConfirm();
          }}
        >
          Delete project
        </button>
      </div>
    </DialogShell>
  );
}
