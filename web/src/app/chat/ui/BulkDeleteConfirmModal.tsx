import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useEffect, useState } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { TeamChip } from "./MoveChatConfirmDialog";
import { fetchConversationOutputs } from "./teamSharing";
import { plural } from "@/app/shared/lib/format";

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

// BulkSharedLoss is one audience's part of a bulk selection (B33 for many):
// the selected chats shared with that team, and the team. Deleting them ends
// the team's access exactly as a single delete does, so the confirm says so —
// a bulk delete must not be the quiet way around the shared-chat warning. A
// selection can span audiences (chats in projects shared with different
// teams), so the modal takes one group per team and names every one.
export type BulkSharedLoss = {
  conversationIds: string[];
  team?: string;
};

// Counting a selection's shared files costs one outputs read per shared chat,
// so it is bounded: at most SHARED_COUNT_CONCURRENCY reads in flight, and past
// MAX_COUNTED_SHARED_CHATS selected shared chats it is not attempted at all —
// the copy says "and their shared files" without a number straight away (the
// same copy a failed count gets) rather than firing dozens of reads and
// holding the confirm on them.
export const MAX_COUNTED_SHARED_CHATS = 25;
export const SHARED_COUNT_CONCURRENCY = 4;

// mapLimited is Promise.all over items with at most `limit` calls in flight.
async function mapLimited<T, R>(items: T[], limit: number, fn: (item: T) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return out;
}

// useSummedSharedFiles sums shared_count over ids: undefined while loading,
// null when any count failed or the selection is too large to count (the copy
// then drops the number), else the sum.
function useSummedSharedFiles(ids: string[] | undefined): number | null | undefined {
  const key = (ids ?? []).join("\n");
  const tooMany = (ids?.length ?? 0) > MAX_COUNTED_SHARED_CHATS;
  const [state, setState] = useState<{ key: string; total: number | null } | null>(null);
  useEffect(() => {
    if (!key || tooMany) return;
    let cancelled = false;
    void mapLimited(key.split("\n"), SHARED_COUNT_CONCURRENCY, (id) =>
      fetchConversationOutputs(id)
        .then((r) => (typeof r.shared_count === "number" ? r.shared_count : null))
        .catch(() => null),
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
  }, [key, tooMany]);
  if (!key) return 0;
  if (tooMany) return null;
  return state?.key === key ? state.total : undefined;
}

export function BulkDeleteConfirmModal({
  count,
  sharedLoss,
  onCancel,
  onConfirm,
}: {
  count: number;
  // The team-shared chats in the selection, grouped by audience; empty or
  // absent when none are shared.
  sharedLoss?: BulkSharedLoss[];
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [remaining, setRemaining] = useState(COUNTDOWN_SECONDS);
  const groups = (sharedLoss ?? []).filter((g) => g.conversationIds.length > 0);
  const sharedIds = groups.length > 0 ? groups.flatMap((g) => g.conversationIds) : undefined;
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
  const heading = tr("Delete {0}?", [plural(count, "conversation")]);

  return (
    <DialogShell
      label={heading}
      scrimLabel={tr("Close bulk delete confirmation")}
      onDismiss={onCancel}
      className="max-w-[26rem] p-5"
    >
      <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        {heading}
      </h2>
      <p className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
        {tr(count === 1 ? "{0} conversation will be removed. This cannot be undone." : "{0} conversations will be removed. This cannot be undone.", [count])}
      </p>
      {sharedIds ? (
        <SharedLossCopy groups={groups} total={count} files={sharedFiles} />
      ) : null}
      <div className="flex items-center justify-end gap-2">
        <button
          type="button"
          className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          onClick={onCancel}
        >
          {tr("Cancel")}</button>
        <button
          type="button"
          disabled={!ready}
          className={[
            "rounded-[var(--radius-md)] px-4 py-2 text-[0.8125rem] font-medium transition",
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
          {ready ? tr("Delete {0}", [count]) : remaining > 0 ? tr("Wait {0}s…", [remaining]) : tr("Checking…")}
        </button>
      </div>
    </DialogShell>
  );
}

// "2 of these are shared with Elcano. Elcano loses access to them and their 5
// shared files. Teammates who branched them keep their copies." — the B33
// copy, counted for a selection. A selection spanning teams names each one:
// "3 of these are shared with Quant and 1 with Ops. They lose access to them
// and their 5 shared files. …" — the file count summed across all of them.
function SharedLossCopy({
  groups,
  total,
  files,
}: {
  groups: BulkSharedLoss[];
  total: number;
  files: number | null | undefined;
}) {
  const shared = groups.reduce((n, g) => n + g.conversationIds.length, 0);
  const first = groups[0];
  const firstCount = first.conversationIds.length;
  const lead = total === 1
    ? rich("This conversation is shared with {0}.", <TeamChip team={first.team} />)
    : <>{rich(firstCount === 1 ? "{0} of these is shared with {1}" : "{0} of these are shared with {1}", firstCount, <TeamChip team={first.team} />)}{groups.slice(1).map((group, index) => (
      <span key={group.team ?? ""}>
        {index === groups.length - 2 ? tr(" and ") : ", "}
        {rich("{0} with {1}", group.conversationIds.length, <TeamChip team={group.team} />)}
      </span>
    ))}.</>;
  const lossMessages = {
    "oneTeams": [
        "They lose access to it. Teammates who branched it keep their copies.",
        "They lose access to it and its {1}. Teammates who branched it keep their copies.",
        "They lose access to it and its shared files. Teammates who branched it keep their copies."
    ],
    "oneTeam": [
        "{0} loses access to it. Teammates who branched it keep their copies.",
        "{0} loses access to it and its {1}. Teammates who branched it keep their copies.",
        "{0} loses access to it and its shared files. Teammates who branched it keep their copies."
    ],
    "manyTeams": [
        "They lose access to them. Teammates who branched them keep their copies.",
        "They lose access to them and their {1}. Teammates who branched them keep their copies.",
        "They lose access to them and their shared files. Teammates who branched them keep their copies."
    ],
    "manyTeam": [
        "{0} loses access to them. Teammates who branched them keep their copies.",
        "{0} loses access to them and their {1}. Teammates who branched them keep their copies.",
        "{0} loses access to them and their shared files. Teammates who branched them keep their copies."
    ]
};
  const loss = shared === 1
    ? groups.length > 1 ? lossMessages.oneTeams : lossMessages.oneTeam
    : groups.length > 1 ? lossMessages.manyTeams : lossMessages.manyTeam;
  const fileMode = files === 0 ? 0 : typeof files === "number" ? 1 : 2;
  return (
    <p data-testid="bulk-delete-shared-loss" className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
      {lead}{" "}
      {rich(loss[fileMode], <TeamChip team={first.team} />, typeof files === "number" ? plural(files, "shared file", "shared files") : "")}
    </p>
  );
}
