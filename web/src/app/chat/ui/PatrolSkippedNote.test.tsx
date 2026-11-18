import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PatrolBanner } from "./PatrolBanner";
import {
  formatSkippedSlot,
  PatrolSkippedNote,
  skippedForApprovalText,
} from "./PatrolSkippedNote";
import type { ChatSchedule } from "./history";

// Three two-hourly slots on one local day, and "now" later that day.
const day = new Date(2026, 10, 18, 12, 0, 0);
const slot = (h: number) => Math.floor(new Date(2026, 10, 18, h, 0, 0).getTime() / 1000);
const now = new Date(2026, 10, 18, 16, 30, 0);

function patrol(over: Partial<ChatSchedule> = {}): ChatSchedule {
  return {
    id: "p1",
    conversation_id: "c1",
    kind: "patrol",
    title: "Deal patrol",
    prompt: "check",
    cron: "0 */2 * * *",
    timezone: "America/New_York",
    status: "active",
    run_count: 3,
    next_run_at: Math.floor(day.getTime() / 1000) + 8 * 3600,
    ...over,
  };
}

describe("skippedForApprovalText", () => {
  it("lists every skipped run when all are known", () => {
    const text = skippedForApprovalText(
      { reason: "approval_pending", count: 3, slots: [slot(12), slot(14), slot(16)] },
      now,
    );
    const [a, b, c] = [12, 14, 16].map((h) => formatSkippedSlot(slot(h), now));
    expect(text).toContain("Patrol on hold: the runs at ");
    for (const t of [a, b, c]) expect(text).toContain(t);
    expect(text).toContain("were skipped because an approval card is waiting. Approve or decline it to resume.");
  });

  it("says 'the run' for a single skip", () => {
    expect(
      skippedForApprovalText({ reason: "approval_pending", count: 1, slots: [slot(12)] }, now),
    ).toBe(
      `Patrol on hold: the run at ${formatSkippedSlot(slot(12), now)} was skipped because an approval card is waiting. Approve or decline it to resume.`,
    );
  });

  it("counts the runs and names the latest when not all are listed", () => {
    expect(
      skippedForApprovalText({ reason: "approval_pending", count: 9, slots: [slot(14), slot(16)] }, now),
    ).toBe(
      `Patrol on hold: 9 runs were skipped, the latest at ${formatSkippedSlot(slot(16), now)}, because an approval card is waiting. Approve or decline it to resume.`,
    );
  });

  it("adds the weekday for a slot on another day", () => {
    const yesterday = Math.floor(new Date(2026, 10, 17, 22, 0, 0).getTime() / 1000);
    expect(formatSkippedSlot(yesterday, now)).not.toBe(
      formatSkippedSlot(slot(22), now),
    );
  });

  it("says nothing without skipped runs or for another reason", () => {
    expect(skippedForApprovalText(undefined, now)).toBeNull();
    expect(skippedForApprovalText({ reason: "approval_pending", count: 0, slots: [] }, now)).toBeNull();
    expect(skippedForApprovalText({ reason: "queue_full", count: 2, slots: [slot(12)] }, now)).toBeNull();
  });
});

describe("PatrolSkippedNote", () => {
  it("renders only on an active patrol with skipped runs", () => {
    const skipped = { reason: "approval_pending", count: 2, slots: [slot(12), slot(14)] };
    const { rerender } = render(<PatrolSkippedNote schedule={patrol({ skipped })} />);
    expect(screen.getByTestId("patrol-skipped-note").textContent).toContain("Patrol on hold");

    rerender(<PatrolSkippedNote schedule={patrol({ skipped, status: "paused" })} />);
    expect(screen.queryByTestId("patrol-skipped-note")).toBeNull();

    rerender(<PatrolSkippedNote schedule={patrol()} />);
    expect(screen.queryByTestId("patrol-skipped-note")).toBeNull();
  });

  it("shows on the Patrol bar under the patrol it belongs to", () => {
    render(
      <PatrolBanner
        schedules={[
          patrol({ skipped: { reason: "approval_pending", count: 3, slots: [slot(12), slot(14), slot(16)] } }),
          patrol({ id: "p2", title: "Other patrol" }),
        ]}
        pendingApprovals={1}
        onRunNow={() => {}}
        onTogglePause={() => {}}
        onHide={() => {}}
      />,
    );
    const notes = screen.getAllByTestId("patrol-skipped-note");
    expect(notes).toHaveLength(1);
    expect(notes[0].closest('[role="region"]')?.textContent).toContain("Deal patrol");
  });
});
