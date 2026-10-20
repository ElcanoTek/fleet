"use client";

// Polling for a running approved call's progress and outcome
// (docs/APPROVAL-PROGRESS.md). For a tool the bundle lists in
// agent_policy.critical_tool_progress, the server records the latest MCP
// progress the call reported on its approval row. An approval runs outside any
// turn, so there is no live stream to carry it: while the card is executing
// and the tab is visible, it asks the one-card approval GET every
// APPROVAL_PROGRESS_POLL_MS for the progress (shown as a bar and "12 of 24 ·
// message") and, once the call has finished, for its outcome — so the card
// settles on its own instead of waiting for Check result.

import { useEffect, useRef } from "react";
import {
  type Approval,
  type ApprovalOutcomePayload,
  approvalStatusFromOutcome,
  resultCardFromOutcome,
  parseApprovalNarrowed,
  parseApprovalProgress,
} from "./history";
import { conversationApprovalApiUrl } from "@/app/lib/conversationApiUrl";

export const APPROVAL_PROGRESS_POLL_MS = 2000;

type PolledCard = ApprovalOutcomePayload & {
  approval_id?: string;
  progress?: unknown;
};

/**
 * Maps one poll answer onto the card, or null when nothing changed (still
 * running with the same progress, or the card is not in the answer).
 */
export function approvalFromPoll(
  current: Approval,
  data: { resolved_approvals?: PolledCard[] } | null | undefined,
): Approval | null {
  const row = data?.resolved_approvals?.find((r) => r.approval_id === current.id);
  if (!row) return null;
  if (row.executing === true) {
    const progress = parseApprovalProgress(row.progress) ?? current.progress;
    // Stop (docs/APPROVAL-STOP.md): a stop pressed in another tab shows here.
    const stopping = current.stopping === true || row.stopping === true;
    const prev = current.progress;
    const sameProgress =
      progress === prev ||
      (prev !== undefined &&
        progress !== undefined &&
        prev.progress === progress.progress &&
        prev.total === progress.total &&
        prev.message === progress.message);
    if (sameProgress && stopping === (current.stopping === true)) {
      return null;
    }
    return { ...current, status: "pending", executing: true, progress, stopping: stopping || undefined };
  }
  const status = approvalStatusFromOutcome(row);
  if (status === null) return null;
  return {
    ...current,
    status,
    resultText: row.result_text,
    resultCard: resultCardFromOutcome(row),
    executing: false,
    progress: undefined,
    // A narrowed approval's record rides the row (docs/APPROVAL-NARROWING.md).
    narrowed: parseApprovalNarrowed(row.narrowed) ?? current.narrowed,
    stopping: undefined,
    // Only a stop the server recorded (docs/APPROVAL-STOP.md).
    stopped: row.stopped === true || undefined,
  };
}

export function useApprovalProgressPoll(
  approval: Approval,
  conversationId: string,
  active: boolean,
  onUpdate: (next: Approval) => void,
): void {
  const latest = useRef({ approval, onUpdate });
  useEffect(() => {
    latest.current = { approval, onUpdate };
  });
  const id = approval.id;
  useEffect(() => {
    if (!active || !conversationId) return undefined;
    const url = conversationApprovalApiUrl(conversationId, id);
    if (!url) return undefined;
    let stopped = false;
    let timer: number | undefined;
    const tick = async () => {
      if (stopped) return;
      if (document.visibilityState === "visible") {
        try {
          const res = await fetch(url, { cache: "no-store" });
          if (res.ok && !stopped) {
            const next = approvalFromPoll(latest.current.approval, await res.json());
            if (next && !stopped) latest.current.onUpdate(next);
          }
        } catch {
          // A failed poll is retried on the next tick; Check result still works.
        }
      }
      if (!stopped) timer = window.setTimeout(() => void tick(), APPROVAL_PROGRESS_POLL_MS);
    };
    timer = window.setTimeout(() => void tick(), APPROVAL_PROGRESS_POLL_MS);
    return () => {
      stopped = true;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [active, conversationId, id]);
}
