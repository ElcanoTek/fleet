import { formatDate, getLanguage, t as tr } from "@/app/lib/i18n";
import type { Approval, ChatSchedule, ChatScheduleSkipped } from "./history";
import { Icon } from "./Icon";

// A scheduled patrol run is skipped, by design, while an approval card waits
// on the patrol's owner: the patrol never stacks turns behind an unanswered
// card. The server reports the skipped slots on the schedule (`skipped`);
// this note on the Patrol bar tells the owner, so a patrol that has quietly
// stopped running is never a surprise.

// The pending cards that do NOT hold a patrol: an email preview and a model
// suggestion (the server's patrolNonBlockingApprovalTools).
const NON_BLOCKING_TOOLS: ReadonlySet<string> = new Set(["preview_email", "suggest_advanced_model"]);

/** How many pending cards hold the chat's patrols. When this drops, the chat
 *  re-reads its schedules so a settled hold's note goes. */
export function countPatrolBlockingApprovals(
  messages: ReadonlyArray<{ approvals?: ReadonlyArray<Pick<Approval, "status" | "tool">> }>,
): number {
  let n = 0;
  for (const m of messages) {
    for (const a of m.approvals ?? []) {
      if (a.status === "pending" && !NON_BLOCKING_TOOLS.has(a.tool)) n += 1;
    }
  }
  return n;
}

function sameLocalDay(a: Date, b: Date): boolean {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}

/** A skipped slot's time in the viewer's zone: "2:00 PM", or "Tue 2:00 PM"
 *  when it was not today. */
export function formatSkippedSlot(unixSec: number, now: Date): string {
  const at = new Date(unixSec * 1000);
  return formatDate(
    at,
    sameLocalDay(at, now)
      ? { hour: "numeric", minute: "2-digit" }
      : { weekday: "short", hour: "numeric", minute: "2-digit" },
  );
}

function joinList(items: string[]): string {
  try {
    return new Intl.ListFormat(getLanguage(), { style: "long", type: "conjunction" }).format(items);
  } catch {
    return items.join(", ");
  }
}

/** The note's sentence, or null when nothing was skipped for an approval. */
export function skippedForApprovalText(
  skipped: ChatScheduleSkipped | undefined,
  now: Date = new Date(),
): string | null {
  if (!skipped || skipped.reason !== "approval_pending") return null;
  const slots = skipped.slots ?? [];
  if (skipped.count <= 0 || slots.length === 0) return null;
  const times = slots.map((s) => formatSkippedSlot(s, now));
  if (skipped.count > slots.length) {
    return tr(
      "Patrol on hold: {0} runs were skipped, the latest at {1}, because an approval card is waiting. Approve or decline it to resume.",
      [skipped.count, times[times.length - 1]],
    );
  }
  if (times.length === 1) {
    return tr(
      "Patrol on hold: the run at {0} was skipped because an approval card is waiting. Approve or decline it to resume.",
      [times[0]],
    );
  }
  return tr(
    "Patrol on hold: the runs at {0} were skipped because an approval card is waiting. Approve or decline it to resume.",
    [joinList(times)],
  );
}

/** One line under a patrol's row on the Patrol bar; renders nothing unless
 *  the patrol's runs are being skipped for an approval card. */
export function PatrolSkippedNote({ schedule }: { schedule: ChatSchedule }) {
  if (schedule.status !== "active") return null;
  const text = skippedForApprovalText(schedule.skipped);
  if (!text) return null;
  return (
    <div
      role="status"
      data-testid="patrol-skipped-note"
      className="mx-2 mb-2 flex items-start gap-2 px-1.5 text-[0.75rem] leading-[1.4] text-[var(--color-warning-text)]"
    >
      <Icon name="pause" className="mt-0.5 size-3 shrink-0 text-[var(--color-warning-icon)]" />
      <span className="min-w-0 flex-1">{text}</span>
    </div>
  );
}
