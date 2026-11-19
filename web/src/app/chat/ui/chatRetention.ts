// When a temporary chat is deleted, and what the interface says beforehand
// (docs/CHAT-RETENTION.md).
//
// The server deletes a temporary chat once it has gone a whole inactivity
// window (CONVERSATION_TTL_DAYS) without activity: `updated_at` older than the
// window. Which chats are temporary is deleteAllUnpinnedScope.ts's one
// predicate; this file is only the clock. The deployment's numbers come from
// GET /api/server-config, so nothing here is a fixed 90.
//
// The warning is the point. A temporary chat used to carry the same mark from
// its first minute to its last, so the mark said nothing about WHEN, and a
// chat simply was not there one day. Now a chat is flagged for the last
// `warningDays` of its window, with the number of days left, in the rail and
// in the chat itself.

import { useEffect, useState } from "react";

export type RetentionPolicy = {
  // Days without activity before a temporary chat is deleted.
  ttlDays: number;
  // Days before that deletion the chat is flagged. 0 = no warning.
  warningDays: number;
  // Most temporary chats one person keeps; past it the least recently active
  // are deleted first. Absent when the server does not say.
  cap?: number;
};

// A server that does not send the numbers: the window is assumed to be the
// default, and there is NO warning — the countdown is a promise about what the
// server will do, so it is only made from what the server said.
export const DEFAULT_TTL_DAYS = 90;
export const DEFAULT_WARNING_DAYS = 0;

const DAY_SECONDS = 86_400;

export function retentionPolicy(cfg: {
  conversationTtlDays?: number;
  conversationExpiryWarningDays?: number;
  conversationUnpinnedCap?: number;
}): RetentionPolicy {
  const ttlDays = cfg.conversationTtlDays ?? DEFAULT_TTL_DAYS;
  const asked = cfg.conversationExpiryWarningDays ?? DEFAULT_WARNING_DAYS;
  // Held inside the window, as the server holds it: a warning as long as the
  // window would flag every chat from the moment it is created.
  const warningDays = Math.max(0, Math.min(asked, ttlDays - 1));
  return { ttlDays, warningDays, cap: cfg.conversationUnpinnedCap };
}

// daysUntilDeletion: whole days left before the sweep may delete a temporary
// chat last active at `updatedAt` (unix seconds), rounded UP so "1" means
// "within a day" and never reads as more time than there is. 0 = already due
// (the hourly sweep has not reached it yet).
export function daysUntilDeletion(updatedAt: number, ttlDays: number, nowSeconds: number): number {
  const left = updatedAt + ttlDays * DAY_SECONDS - nowSeconds;
  return left <= 0 ? 0 : Math.ceil(left / DAY_SECONDS);
}

// expiresInDays: the days left for a TEMPORARY chat that is inside the
// warning window, or null when it is not yet (or the warning is off). The
// caller has already decided the chat is temporary.
export function expiresInDays(updatedAt: number, policy: RetentionPolicy, nowSeconds: number): number | null {
  if (policy.warningDays <= 0) return null;
  const days = daysUntilDeletion(updatedAt, policy.ttlDays, nowSeconds);
  return days <= policy.warningDays ? days : null;
}

// How close to the cap a person is told about it: within a tenth of it. Never
// with no temporary chats at all — a cap of 1 would otherwise put the
// threshold at zero and warn an empty account for good.
export function nearCap(temporaryCount: number, cap: number | undefined): boolean {
  if (!cap || cap <= 0 || temporaryCount <= 0) return false;
  return temporaryCount >= cap - Math.max(1, Math.floor(cap / 10));
}

// useNowSeconds: the current time in unix seconds, refreshed every `everyMs`
// (hourly by default — the countdown is in days), so a tab left open for a
// week does not keep quoting last week's numbers.
export function useNowSeconds(everyMs = 3_600_000): number {
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000));
  useEffect(() => {
    const id = window.setInterval(() => setNow(Math.floor(Date.now() / 1000)), everyMs);
    return () => window.clearInterval(id);
  }, [everyMs]);
  return now;
}
