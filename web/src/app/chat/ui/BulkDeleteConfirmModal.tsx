"use client";

import { useEffect, useState } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { TeamChip } from "./MoveChatConfirmDialog";
import { fetchConversationOutputs, plural } from "./teamSharing";

// BulkDeleteConfirmModal is the multi-select bulk-delete confirmation (#279).
// It shows the exact selection count and disables the confirm button for a
// 3-second window (with a visible countdown) so an impulsive bulk wipe can't
// fire the instant the modal opens. Cancel is always available.
//
// The parent mounts this component only while the modal is open, so the
// countdown state initializes fresh (to COUNTDOWN_SECONDS) on each open — no
// reset logic is needed inside an effect, and there's no cascading-render
// hazard.
const COUNTDOWN_SECONDS = 3;

// SharedLoss is the team-shared part of a bulk selection (B33 for many): the
// chats that are shared with the team, and that team. Deleting them ends the
// team's access exactly as a single delete does, so the confirm says so —
// a bulk delete must not be the quiet way around the shared-chat warning.
export type BulkSharedLoss = {
  conversationIds: string[];
  team?: string;
};

// useSummedSharedFiles sums shared_count over ids: undefined while loading,
// null when any count failed (the copy then drops the number), else the sum.
function useSummedSharedFiles(ids: string[] | undefined): number | null | undefined {
  const key = (ids ?? []).join("\n");
  const [state, setState] = useState<{ key: string; total: number | null } | null>(null);
  useEffect(() => {
    if (!key) return;
    let cancelled = false;
    void Promise.all(
      key.split("\n").map((id) =>
        fetchConversationOutputs(id)
          .then((r) => (typeof r.shared_count === "number" ? r.shared_count : null))
          .catch(() => null),
      ),
    ).then((counts) => {
      if (cancelled) return;
      const total = counts.some((c) => c === null)
        ? null
        : counts.reduce<number>((a, c) => a + (c ?? 0), 0);
      setState({ key, total });
    });
    return () => {
      cancelled = true;
    };
  }, [key]);
  if (!key) return 0;
  return state?.key === key ? state.total : undefined;
}

export function BulkDeleteConfirmModal({
  count,
  sharedLoss,
  onCancel,
  onConfirm,
}: {
  count: number;
  // The team-shared chats in the selection, if any.
  sharedLoss?: BulkSharedLoss;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [remaining, setRemaining] = useState(COUNTDOWN_SECONDS);
  const sharedIds =
    sharedLoss && sharedLoss.conversationIds.length > 0 ? sharedLoss.conversationIds : undefined;
  const sharedFiles = useSummedSharedFiles(sharedIds);

  useEffect(() => {
    const start = Date.now();
    const id = window.setInterval(() => {
      const elapsed = (Date.now() - start) / 1000;
      setRemaining(Math.max(0, Math.ceil(COUNTDOWN_SECONDS - elapsed)));
    }, 100);
    return () => window.clearInterval(id);
  }, []);

  // Held until the shared-file count is known (or known to have failed), so
  // the confirm never fires over copy that is still missing its number.
  const ready = remaining <= 0 && sharedFiles !== undefined;
  // The heading is also the dialog's accessible name, so it is built once.
  const heading = `Delete ${count} conversation${count === 1 ? "" : "s"}?`;

  return (
    <DialogShell
      label={heading}
      scrimLabel="Close bulk delete confirmation"
      onDismiss={onCancel}
      className="max-w-[26rem] p-5"
    >
      <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        {heading}
      </h2>
      <p className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
        {count} conversation{count === 1 ? "" : "s"} will be removed. This cannot be
        undone.
      </p>
      {sharedIds ? (
        <SharedLossCopy
          shared={sharedIds.length}
          total={count}
          team={sharedLoss?.team}
          files={sharedFiles}
        />
      ) : null}
      <div className="flex items-center justify-end gap-2">
        <button
          type="button"
          className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          onClick={onCancel}
        >
          Cancel
        </button>
        <button
          type="button"
          disabled={!ready}
          className={[
            "rounded-full px-4 py-2 text-[0.8125rem] font-medium transition",
            // The foreground travels with the fill. --color-surface-1 is the
            // theme-aware readable foreground on a saturated fill (dark in the
            // dark theme, white in the light one) — white was 2.77:1 on the
            // dark theme's #e08080. The countdown state is a genuinely
            // disabled control, so it takes the muted treatment rather than a
            // high-contrast label on a half-alpha fill.
            ready
              ? "bg-[var(--color-danger)] text-[var(--color-surface-1)] hover:opacity-90"
              : "cursor-not-allowed bg-[var(--color-danger)]/50 text-[var(--color-text-muted)]",
          ].join(" ")}
          onClick={onConfirm}
        >
          {ready ? `Delete ${count}` : remaining > 0 ? `Wait ${remaining}s…` : "Checking…"}
        </button>
      </div>
    </DialogShell>
  );
}

// "2 of these are shared with Elcano. Elcano loses access to them and their 5
// shared files. Teammates who branched them keep their copies." — the B33
// copy, counted for a selection.
function SharedLossCopy({
  shared,
  total,
  team,
  files,
}: {
  shared: number;
  total: number;
  team?: string;
  files: number | null | undefined;
}) {
  const one = shared === 1;
  const lead =
    total === 1
      ? "This conversation is shared with"
      : `${shared} of these ${one ? "is" : "are"} shared with`;
  const them = one ? "it" : "them";
  const their = one ? "its" : "their";
  const filesPart =
    files === 0
      ? ""
      : typeof files === "number"
        ? ` and ${their} ${plural(files, "shared file", "shared files")}`
        : ` and ${their} shared files`;
  return (
    <p
      data-testid="bulk-delete-shared-loss"
      className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]"
    >
      {lead} <TeamChip team={team} suffix="." /> <TeamChip team={team} /> loses
      access to {them}
      {filesPart}. Teammates who branched {them} keep their copies.
    </p>
  );
}
