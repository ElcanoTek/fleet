"use client";

import { useEffect, useState } from "react";
import { fetchConversationOutputs } from "./teamSharing";

/** The owner's per-file share states for one chat (B17's markers). */
export type OutputShareStates = {
  id: string;
  shared: Map<string, boolean>;
};

/**
 * useOutputShareStates reads GET /conversations/{id}/outputs for the owner's
 * team-shared chat and keeps the per-file Shared / Not shared states the
 * output chips' markers show. It re-reads:
 *
 *   - when the chat gains a message (`messageCount`) — a reply may present a
 *     new output;
 *   - when the active turn SETTLES (`turnActive` going false). The message
 *     count moves at send time, before the reply exists, so keying on it
 *     alone left a file presented in the just-finished reply with no marker;
 *     the read is held while the turn runs and happens once it ends;
 *   - whenever `paused` clears — the share dialog or the project home closing
 *     (the dialog's checklist and Sources' toggles change the states).
 *
 * A failed read drops the markers rather than leaving the previous ones
 * standing as if they were current. Disabled (a private chat) it reads
 * nothing and returns whatever it last had for another id — callers match on
 * `id`.
 */
export function useOutputShareStates({
  conversationId,
  enabled,
  paused,
  turnActive,
  messageCount,
}: {
  conversationId: string | null;
  enabled: boolean;
  paused: boolean;
  turnActive: boolean;
  messageCount: number;
}): OutputShareStates | null {
  const [states, setStates] = useState<OutputShareStates | null>(null);
  useEffect(() => {
    if (!enabled || !conversationId || paused || turnActive) {
      return;
    }
    let cancelled = false;
    const id = conversationId;
    fetchConversationOutputs(id)
      .then((res) => {
        if (cancelled) return;
        setStates({
          id,
          shared: new Map(res.outputs.map((o) => [o.path, o.shared])),
        });
      })
      .catch(() => {
        // No markers rather than wrong ones: this refresh exists because the
        // states may have changed (a reply, the share dialog, Sources), so a
        // failed read must not leave the PREVIOUS map's Shared / Not shared
        // labels standing as if they were current.
        if (cancelled) return;
        setStates((prev) => (prev?.id === id ? null : prev));
      });
    return () => {
      cancelled = true;
    };
  }, [enabled, conversationId, paused, turnActive, messageCount]);
  return states;
}
