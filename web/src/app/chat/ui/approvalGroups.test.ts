import { describe, expect, it } from "vitest";
import type { Approval } from "./history";
import { approvalRenderItems, earliestExpiry } from "./approvalGroups";

// Grouped approvals (docs/GROUPED-APPROVALS.md): two or more pending cards of
// one turn's group render as one card; everything else renders on its own.

function ap(id: string, extra: Partial<Approval> = {}): Approval {
  return { id, tool: "mcp_x_execute_plan", summary: {}, status: "pending", ...extra };
}

describe("approvalRenderItems", () => {
  it("groups two or more pending cards of a group at the first card's place", () => {
    const items = approvalRenderItems([
      ap("before"),
      ap("a", { groupId: "g1" }),
      ap("mid"),
      ap("b", { groupId: "g1" }),
    ]);
    expect(items.map((i) => (i.kind === "group" ? `group:${i.approvals.map((a) => a.id).join(",")}` : i.approval.id))).toEqual([
      "before",
      "group:a,b",
      "mid",
    ]);
  });

  it("leaves a lone group member, a running one and a settled one as individual cards", () => {
    const items = approvalRenderItems([
      ap("a", { groupId: "g1" }),
      ap("b", { groupId: "g1", executing: true }),
      ap("c", { groupId: "g1", status: "approved" }),
      ap("d", { groupId: "g2" }),
    ]);
    expect(items.every((i) => i.kind === "single")).toBe(true);
    expect(items).toHaveLength(4);
  });

  it("renders a split group one card at a time, and keeps other groups grouped", () => {
    const list = [ap("a", { groupId: "g1" }), ap("b", { groupId: "g1" }), ap("c", { groupId: "g2" }), ap("d", { groupId: "g2" })];
    const items = approvalRenderItems(list, new Set(["g1"]));
    expect(items.map((i) => i.kind)).toEqual(["single", "single", "group"]);
  });

  it("changes nothing for cards without a group", () => {
    const list = [ap("a"), ap("b"), ap("c")];
    expect(approvalRenderItems(list)).toEqual(list.map((approval) => ({ kind: "single", approval })));
  });
});

describe("earliestExpiry", () => {
  it("is the soonest deadline, ignoring cards that never expire", () => {
    expect(earliestExpiry([ap("a", { expiresAt: 0 }), ap("b", { expiresAt: 900 }), ap("c", { expiresAt: 600 })])).toBe(600);
    expect(earliestExpiry([ap("a"), ap("b", { expiresAt: 0 })])).toBe(0);
  });
});
