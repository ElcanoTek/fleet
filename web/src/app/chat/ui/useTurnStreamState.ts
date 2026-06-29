import { useRef, useState } from "react";

// useTurnStreamState groups the per-conversation turn/SSE transport state
// that used to live inline in ChatExperience (issue #401). Every ref here
// is keyed by conv slot (a real conversation id, or a per-submission
// pending key until the server promotes it) and its entire lifecycle —
// read, write, and the pending→real rename — is internal to turn
// streaming. That is the boundary: the message store (messagesByConv +
// its mirror ref and helpers), the active-conversation pointer
// (activeConversationIdRef), and the latest-callback refs deliberately stay
// in the component, because they're owned by other concerns and are merely
// *consumed* by the stream loop. A future `useTurnStream` would absorb this
// hook's internals and take those component-owned values as inputs.
//
// This is a mechanical relocation: every body below ran in the component
// before, unchanged, so behavior is preserved.
import type { RefObject } from "react";

export type TurnStreamState = {
  // streamingConvs tracks every conversation slot that currently has a turn
  // in flight (sidebar "working" dots read it). Mirrored into a ref so
  // synchronous code paths (event handlers, finally blocks) can read current
  // membership without waiting for the next render.
  streamingConvs: Set<string>;
  streamingConvsRef: RefObject<Set<string>>;
  // Derived: the conv the user is currently looking at has a turn in flight.
  isStreaming: boolean;
  markConvStreaming: (key: string) => void;
  markConvIdle: (key: string) => void;
  renameStreamingKey: (oldKey: string, newKey: string) => void;
  // Abort controllers keyed by the conv slot whose POST /chat we're
  // streaming. Multiple chats can be in flight at once; the Stop button only
  // aborts the controller for the conv the user is currently looking at.
  abortControllersRef: RefObject<Record<string, AbortController>>;
  // Conv ids this client currently has an SSE socket attached to.
  attachedConvIdsRef: RefObject<Set<string>>;
  // Per-conv last applied SSE event id + current turn id — the persistent
  // state behind the stepStreamDedup reducer (replay/idempotency on reattach).
  lastEventIdByConvRef: RefObject<Record<string, number>>;
  currentTurnIdByConvRef: RefObject<Record<string, string>>;
  // Guard against concurrent reattach attempts for the same conv.
  reattachInFlightRef: RefObject<Set<string>>;
  // Promote a pending key onto the real conv id across the streaming set,
  // the attached set, and the abort-controller map in one synchronous step.
  // Mirrors promoteComposerKey; the conversation-event handler calls both
  // back-to-back so no SSE event can observe a half-renamed state.
  promoteStreamKey: (oldKey: string, newKey: string) => void;
};

export function useTurnStreamState(currentConvKey: string): TurnStreamState {
  // streamingConvs tracks every conversation slot that currently has a
  // turn in flight — keyed by conv id (or a pending key for a brand-new
  // chat whose server id we haven't heard back yet). Multiple entries =
  // multiple chats running in parallel. The sidebar reads this to paint a
  // "working" dot next to each in-flight chat, and the active conv's
  // composer uses the derived `isStreaming` below to gate input.
  const [streamingConvs, setStreamingConvs] = useState<Set<string>>(
    () => new Set<string>(),
  );
  // Ref mirror so synchronous code paths (event handlers, finally blocks)
  // can read current membership without waiting for the next render.
  const streamingConvsRef = useRef<Set<string>>(new Set<string>());
  const markConvStreaming = (key: string) => {
    if (streamingConvsRef.current.has(key)) return;
    streamingConvsRef.current.add(key);
    setStreamingConvs(new Set(streamingConvsRef.current));
  };
  const markConvIdle = (key: string) => {
    if (!streamingConvsRef.current.has(key)) return;
    streamingConvsRef.current.delete(key);
    setStreamingConvs(new Set(streamingConvsRef.current));
  };
  const renameStreamingKey = (oldKey: string, newKey: string) => {
    if (!streamingConvsRef.current.has(oldKey)) return;
    streamingConvsRef.current.delete(oldKey);
    streamingConvsRef.current.add(newKey);
    setStreamingConvs(new Set(streamingConvsRef.current));
  };

  // Derived from streamingConvs: true when the conv the user is currently
  // looking at has a turn in flight. Drives the composer disabled states,
  // Stop button visibility, auto-scroll behavior, etc. Other conversations
  // may also be streaming simultaneously — see streamingConvs and the
  // sidebar dot indicator.
  const isStreaming = streamingConvs.has(currentConvKey);

  // Abort controllers keyed by the conv slot whose POST /chat we're
  // streaming. Multiple chats can be in flight at once; the Stop button
  // (and clearConversation) only aborts the controller for the conv the
  // user is currently looking at. A pending key is used until the
  // server promotes the slot to a real id.
  const abortControllersRef = useRef<Record<string, AbortController>>({});
  // Conv ids this client currently has an SSE socket attached to. A pending
  // key = attached to a new chat whose server-side id we haven't heard back
  // yet; otherwise a real conversation id. Multiple entries means we're
  // draining live streams from more than one chat in parallel.
  const attachedConvIdsRef = useRef<Set<string>>(new Set<string>());
  // Per-conversation last applied SSE event id. Updated whenever the
  // dispatch loop commits an event. On reattach we send this value as
  // Last-Event-ID so the server replays everything AFTER it and we pick up
  // without duplicating already-applied state. Event IDs are monotonic
  // WITHIN A TURN but reset between turns, so we also track the current turn
  // id per conv to reset lastEventId when a new turn begins.
  const lastEventIdByConvRef = useRef<Record<string, number>>({});
  const currentTurnIdByConvRef = useRef<Record<string, string>>({});
  // Guard for concurrent reattach attempts per conv. Without it, two rapid
  // visibilitychange events (unlock + focus) would open two /stream sockets
  // and render every event twice.
  const reattachInFlightRef = useRef<Set<string>>(new Set());

  // promoteStreamKey migrates every pending-keyed handle onto the real conv
  // id once the "conversation" SSE event lands, so subsequent reads (Stop
  // button, attached-set membership, the streaming-set membership the
  // sidebar reads) all point at the same slot the SSE events are now writing
  // to. Kept synchronous and mutation-in-place on the refs (as before) so a
  // concurrent SSE handler reading a ref sees the renamed key immediately.
  const promoteStreamKey = (oldKey: string, newKey: string) => {
    if (attachedConvIdsRef.current.has(oldKey)) {
      attachedConvIdsRef.current.delete(oldKey);
      attachedConvIdsRef.current.add(newKey);
    }
    const pendingController = abortControllersRef.current[oldKey];
    if (pendingController) {
      delete abortControllersRef.current[oldKey];
      abortControllersRef.current[newKey] = pendingController;
    }
    renameStreamingKey(oldKey, newKey);
  };

  return {
    streamingConvs,
    streamingConvsRef,
    isStreaming,
    markConvStreaming,
    markConvIdle,
    renameStreamingKey,
    abortControllersRef,
    attachedConvIdsRef,
    lastEventIdByConvRef,
    currentTurnIdByConvRef,
    reattachInFlightRef,
    promoteStreamKey,
  };
}
