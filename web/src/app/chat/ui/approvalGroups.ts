// Grouped approvals (docs/GROUPED-APPROVALS.md).
//
// A bundle can list critical tools in agent_policy.critical_tool_group_approval.
// The server then stamps every card one turn stages for such a tool with the
// turn's group id (Approval.groupId). This module decides how a message's
// approval cards render: two or more PENDING cards that share a group id (and
// are not running yet) become one grouped card, placed where the first of them
// would have rendered; every other card renders on its own, exactly as before.
// Once a card is decided (running, or settled), it leaves the group and shows
// its own outcome card. A group the person split with "One at a time" renders
// its cards individually.

import type { Approval } from "./history";
import { approvalIsExecuting } from "./history";

export type ApprovalRenderItem =
  | { kind: "single"; approval: Approval }
  | { kind: "group"; groupId: string; approvals: Approval[] };

function groupable(a: Approval, split: ReadonlySet<string>): a is Approval & { groupId: string } {
  return (
    typeof a.groupId === "string" &&
    a.groupId !== "" &&
    a.status === "pending" &&
    !approvalIsExecuting(a) &&
    !split.has(a.groupId)
  );
}

export function approvalRenderItems(
  approvals: readonly Approval[],
  split: ReadonlySet<string> = new Set(),
): ApprovalRenderItem[] {
  const members = new Map<string, Approval[]>();
  for (const a of approvals) {
    if (!groupable(a, split)) continue;
    members.set(a.groupId, [...(members.get(a.groupId) ?? []), a]);
  }
  const items: ApprovalRenderItem[] = [];
  const placed = new Set<string>();
  for (const a of approvals) {
    if (groupable(a, split) && (members.get(a.groupId)?.length ?? 0) >= 2) {
      if (!placed.has(a.groupId)) {
        placed.add(a.groupId);
        items.push({ kind: "group", groupId: a.groupId, approvals: members.get(a.groupId)! });
      }
      continue;
    }
    items.push({ kind: "single", approval: a });
  }
  return items;
}

// earliestExpiry is the group's countdown deadline: the soonest expires_at of
// its cards (0 = none of them expires).
export function earliestExpiry(approvals: readonly Approval[]): number {
  let min = 0;
  for (const a of approvals) {
    const e = a.expiresAt ?? 0;
    if (e > 0 && (min === 0 || e < min)) min = e;
  }
  return min;
}
