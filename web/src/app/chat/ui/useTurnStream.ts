import type { Dispatch, RefObject, SetStateAction } from "react";
import { useRef, useState } from "react";
import {
  applyContextCompacted,
  applyContextPressure,
  applyModelRequired,
  applyRetryNotice,
  applySubagentProgress,
  clearRetryNotice,
  historyToMessages,
  parsePythonStream,
  type Approval,
  type ApprovalStatus,
  type ContextCompactedEventPayload,
  type ContextPressureEventPayload,
  type HistoryEntry,
  type Message,
  type ModelRequiredEventPayload,
  type RetryEventPayload,
  type SubagentProgressEventPayload,
  type ToolCallState,
} from "./history";
import { parseSseChunk, stepStreamDedup, type ServerEvent } from "@/app/lib/sse";
import { currentDefaultModel } from "@/app/lib/modelAliases";
import { PENDING_CONV_KEY } from "./workspaceHref";
import { mcpAccountOverrides } from "./mcpAccounts";
import { enabledOptionalMcpServerNames } from "./mcpSelection";

// One pending input in a conversation's #785 queue (wire shape of
// queue.updated / GET /queue items).
export type QueuedInput = {
  id: string;
  client_input_id: string;
  mode: "queued" | "steer";
  state: string;
  position: number;
  message_preview: string;
  has_attachments: boolean;
};
import { formatBytes } from "./formatters";
import type { ConversationSummary, MCPServerInfo } from "./chat-experience";
import type { PerConvComposerState } from "./usePerConvComposerState";
import type { TurnStreamState } from "./useTurnStreamState";

// Does the conversation still owe the user a turn? `queued` rows are waiting
// for a drain kick; `running` rows were claimed by one (their turn may not
// have registered yet). `injected` rows are NOT drain work — they were folded
// into a turn that is already generating and complete with it.
export function hasPendingQueueWork(items: QueuedInput[] | null | undefined): boolean {
  return (items ?? []).some((it) => it.state === "queued" || it.state === "running");
}

// Backoff for the post-turn queue handoff (#785). The server drains the
// queue from the finishing turn's own tail call, so the usual case resolves on
// the first attempt; the later steps cover a drain that had to be re-kicked
// (concurrency cap full, race-loser un-claim — 2-3s server-side timers). The
// schedule is bounded on purpose: a row that is still queued after it runs out
// is not draining on its own (a restart leaves rows queued deliberately — boot
// recovery never auto-drains), and the honest answer then is an accurate chip
// strip with a send-now button, not an endless poll.
export const queueDrainFollowDelaysMs = [250, 500, 1000, 2000, 3000, 4000];

// useTurnStream owns the live chat turn/SSE loop that used to sit inline in
// ChatExperience (issue #401 step 3 / #435). It is the BEHAVIOR half of the
// turn machine; the per-conversation transport STATE it drives stays grouped
// in useTurnStreamState, and the per-conversation composer state in
// usePerConvComposerState — both are still instantiated by the component
// (their values are read during render and by component-level effects) and
// flow into this hook as `deps`. That state/behavior split is why this hook
// receives useTurnStreamState's refs/setters as inputs rather than calling
// useTurnStreamState itself.
//
// This is a behavior-preserving RELOCATION, not a rewrite: the nine callback
// bodies below are byte-for-byte the same code that ran in the component.
// The only structural change is that the values they close over arrive via a
// single typed `deps` object destructured into the SAME local names — so a
// reviewer can diff the moved bodies (git --color-moved) and see they are
// unchanged. `deps` is a fresh object literal every render and the callbacks
// are plain (non-memoized) consts recreated every render, exactly as before,
// so closure freshness is identical. Do NOT wrap these in useCallback or
// memoize `deps`: that would change behavior and is not what this lift is.
//
// Mutual recursion across the boundary (loadConversation ↔ reattachToConv)
// is handled by the component's existing latest-callback refs: this hook
// takes loadConversation as a dep and returns reattachToConv, which the
// component syncs into reattachToConvRef for loadConversation to call.

// Wall-clock read isolated in a module-level helper (mirrors the same helper
// in chat-experience.tsx): the async stream handlers run during a render pass
// for the React Compiler's lint rules, so a bare Date.now() there trips
// react-hooks/purity. These timestamps are only elapsed-time math and local
// message ids, never render-affecting derived state.
const nowMs = (): number => Date.now();

const minimumThinkingMs = 250;
const streamIdleTimeoutMs = 300000;

// ── Liveness thresholds (checkStreamLiveness) ───────────────────────────────
//
// The server heartbeats an attached stream every FLEET_SSE_HEARTBEAT_INTERVAL
// (15s by default), so a healthy socket is almost never silent for long. But
// the heartbeat is operator-configurable and could be off entirely, so nothing
// below TREATS silence as proof of death — silence only decides whether it is
// worth spending a probe. Death is proven by the server having moved past us
// while our socket produces nothing.

// How long an attached stream must have produced no bytes before the watchdog
// spends an /inflight probe on it. Derived from the cadence the server
// advertises (X-Fleet-Heartbeat-Interval-Ms) so it tracks the deployment
// rather than a hard-coded guess: an attached stream writes at least one byte
// per interval, so a stream still inside one interval is demonstrably alive
// and not worth a request. Explicit tab returns bypass this.
const streamSilenceProbeMs = (heartbeatMs: number): number =>
  heartbeatMs > 0 ? Math.max(2 * heartbeatMs, 5000) : 20000;

// How long a stream may be silent before silence ALONE is proof it is dead.
//
// This is the difference between a socket that stops mid-answer and one that
// stops while the agent is thinking. Event-based evidence — "the server has
// emitted past what we applied" — never materializes during a long tool call,
// because the server has nothing to emit. The keepalive does: every interval,
// on an attached stream, unconditionally. Miss several in a row while the tab
// is awake and the connection is gone, whatever the turn is doing.
//
// Four intervals (a full minute at the 15s default) is deliberately generous:
// three consecutive keepalives can be lost to a GC pause or a slow paint
// without the socket being dead, and being late costs the user a few seconds
// while being early costs a needless reconnect. Returns Infinity when the
// server reports keepalives disabled — with no promised cadence, silence
// proves nothing and only event-based evidence counts.
const streamDeadSilenceMs = (heartbeatMs: number): number =>
  heartbeatMs > 0 ? Math.max(4 * heartbeatMs, 15000) : Number.POSITIVE_INFINITY;

// After the probe says "the turn is alive and has emitted past you", how long
// to let the socket prove itself before declaring it dead. A socket that was
// merely frozen (backgrounded tab, suspended process) delivers its buffered
// bytes well inside this window; a severed one delivers nothing, ever.
const streamLivenessGraceMs = 2500;

// Ceiling on waiting for a superseded stream's own teardown to unwind before
// the replacement attaches. Bounded so a wedged unwind degrades to "no
// reconnect this round" (the watchdog retries) instead of hanging.
const supersedeUnwindTimeoutMs = 2000;

// Small awaited delay. Isolated like nowMs so the async stream handlers keep
// clear of the React Compiler's purity rules.
const delay = (ms: number) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));

// Classifies the /api/chat response to a mode:"queue" submission (#824).
// The server only honors queueing while a turn is actually RUNNING; if our
// busy flag was stale (the turn finished between our check and the server's),
// the same POST starts a direct turn and the response is a live SSE stream,
// not a JSON queue ack. Treating that as an ack leaves a billed turn running
// with nobody reading it — no user bubble, no stream, nothing in the queue.
//   "queued" — JSON ack (202 accepted, or 200 idempotent replay)
//   "stream" — a live turn started on this response; hand off to the
//              reattach path instead of ignoring the body
//   "error"  — refused (429 queue full, 5xx, …); give the text back
export function classifyQueueSubmitResponse(res: {
  ok: boolean;
  status: number;
  headers: { get(name: string): string | null };
}): "queued" | "stream" | "error" {
  if (!res.ok && res.status !== 202) return "error";
  const contentType = res.headers.get("content-type") ?? "";
  return contentType.toLowerCase().includes("text/event-stream") ? "stream" : "queued";
}

// persistedAnswersLocalTurn reports whether the CANONICAL (Postgres) copy of a
// conversation already contains a finished assistant reply for the turn the
// client is still holding open — i.e. whether hitting refresh right now would
// show the answer.
//
// Two conditions, both load-bearing:
//   - the persisted transcript ends in a completed, non-failed assistant
//     message (the reply landed and the turn was sealed), and
//   - it covers at least as many user turns as our in-memory copy.
//
// The second guard is what keeps this from adopting a STALE transcript. A
// conversation whose previous turn completed also "ends in an assistant
// reply"; without the user-turn count we would happily swap the live turn's
// prompt out of the transcript and call it a recovery. Counting user messages
// (not entries) is the comparison that survives historyToMessages' grouping:
// it merges an assistant's text/tool_call/tool_result rows into one message
// but never merges user rows.
export function persistedAnswersLocalTurn(
  history: HistoryEntry[] | null | undefined,
  localMessages: Message[],
): boolean {
  const persisted = historyToMessages(history ?? []);
  const last = persisted[persisted.length - 1];
  if (!last || last.role !== "assistant" || last.state !== "done" || last.failed) {
    return false;
  }
  const userTurns = (messages: Message[]) =>
    messages.reduce((n, m) => n + (m.role === "user" ? 1 : 0), 0);
  return userTurns(persisted) >= userTurns(localMessages);
}

// Server-trusted attachment metadata returned by POST /api/attachments and
// forwarded in the /api/chat body. Local to the turn loop.
type UploadedAttachmentMeta = {
  name: string;
  path: string;
  size: number;
  mime?: string;
};

// TurnStreamDeps is the complete, typed surface the moved loop bodies read or
// call. Composer- and transport-state members reuse the source hooks' types
// (indexed access) so the assembly site's `satisfies TurnStreamDeps` flags any
// drift at the point of omission, not just inside a body. Every field here is
// referenced by at least one moved body.
export interface TurnStreamDeps {
  // Per-conversation message store + assorted component callbacks.
  setConvMessages: (
    convId: string,
    updater: Message[] | ((prev: Message[]) => Message[]),
  ) => void;
  getConvMessages: (convId: string) => Message[];
  renameConvKey: (oldKey: string, newKey: string) => void;
  patchAssistantMessage: (
    convId: string,
    assistantId: number,
    updater: (message: Message) => Message,
  ) => void;
  startThinkingCrossfade: (assistantId: number) => void;
  refreshConversations: () => Promise<void>;
  loadConversation: (
    conversationId: string,
    options?: { preserveScroll?: boolean; background?: boolean; restore?: boolean },
  ) => Promise<void>;
  loadMemories: () => Promise<void>;
  loadRankedModels: () => Promise<void>;
  loadCatalogModels: () => Promise<void>;
  nextPendingKey: () => string;
  isPendingKey: (key: string | null) => boolean;
  // Composer helpers (usePerConvComposerState).
  setPromptForKey: PerConvComposerState["setPromptForKey"];
  setPendingAttachmentsForKey: PerConvComposerState["setPendingAttachmentsForKey"];
  setAttachmentErrorForKey: PerConvComposerState["setAttachmentErrorForKey"];
  markConvUploading: PerConvComposerState["markConvUploading"];
  markConvUploadDone: PerConvComposerState["markConvUploadDone"];
  getPendingAttachmentsForKey: PerConvComposerState["getPendingAttachmentsForKey"];
  promoteComposerKey: PerConvComposerState["promoteComposerKey"];
  // Component state setters.
  setMessagesByConv: Dispatch<SetStateAction<Record<string, Message[]>>>;
  setConversations: Dispatch<SetStateAction<ConversationSummary[]>>;
  setActiveConversationId: Dispatch<SetStateAction<string | null>>;
  setSelectedPersona: Dispatch<SetStateAction<string>>;
  setSelectedModel: Dispatch<SetStateAction<string>>;
  setModelPickerOpen: Dispatch<SetStateAction<boolean>>;
  setModelSearchQuery: Dispatch<SetStateAction<string>>;
  setPendingLockdown: Dispatch<SetStateAction<boolean>>;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
  setSpreadsheetNudgeDismissed: Dispatch<SetStateAction<boolean>>;
  // Component-owned refs the loop reads/mutates.
  activeConversationIdRef: RefObject<string | null>;
  messagesByConvRef: RefObject<Record<string, Message[]>>;
  pendingApprovalScrollRef: RefObject<string | null>;
  // Component state values (read-only in the loop).
  selectedModel: string;
  selectedPersona: string;
  mcpServers: MCPServerInfo[];
  pendingLockdown: boolean;
  userEmail: string;
  modelError: { message: string; modelsUrl: string } | null;
  // Turn-stream transport state (useTurnStreamState).
  markConvStreaming: TurnStreamState["markConvStreaming"];
  markConvIdle: TurnStreamState["markConvIdle"];
  abortControllersRef: TurnStreamState["abortControllersRef"];
  attachedConvIdsRef: TurnStreamState["attachedConvIdsRef"];
  lastEventIdByConvRef: TurnStreamState["lastEventIdByConvRef"];
  currentTurnIdByConvRef: TurnStreamState["currentTurnIdByConvRef"];
  reattachInFlightRef: TurnStreamState["reattachInFlightRef"];
  streamPulseRef: TurnStreamState["streamPulseRef"];
  serverHeartbeatMsRef: TurnStreamState["serverHeartbeatMsRef"];
  supersededStreamsRef: TurnStreamState["supersededStreamsRef"];
  livenessInFlightRef: TurnStreamState["livenessInFlightRef"];
  promoteStreamKey: TurnStreamState["promoteStreamKey"];
  streamingConvsRef: TurnStreamState["streamingConvsRef"];
  isStreaming: boolean;
}

// The public entry points the component/JSX still call. applyStreamEvent,
// pumpStreamResponse, streamTurn, and uploadPendingAttachments are internal
// to the loop and intentionally not returned.
export interface UseTurnStream {
  // Resolves true when this call attached to a turn and pumped its stream
  // (so the caller knows the conversation was, and may still be, ours).
  reattachToConv: (convId: string) => Promise<boolean>;
  // Tab-return / watchdog recovery for a socket that died while the device
  // slept: adopt the persisted transcript when the turn has since finished,
  // or reconnect the live stream when it is still generating. `force` skips
  // the silence gate (use it for explicit tab returns, not periodic ticks).
  checkStreamLiveness: (
    convId: string,
    opts?: { force?: boolean },
  ) => Promise<"idle" | "healthy" | "recovered" | "reconnected">;
  // checkStreamLiveness across every attached conversation, not just the
  // active one. No-op while the tab is hidden.
  sweepStreamLiveness: (opts?: { force?: boolean }) => Promise<void>;
  submitPrompt: (submittedPrompt: string) => Promise<void>;
  regenerateLastAssistant: () => Promise<void>;
  resendUserMessage: (userMessageId: number, editedContent: string) => Promise<void>;
  retryLastUserMessage: () => Promise<void>;
  // #785 pending-input queue: per-conversation snapshot + mutations.
  //
  // A Map, not a Record: the key is a conversation id that arrives from the URL
  // and from stream events, and writing it as a computed property on a plain
  // object is the remote-property-injection shape CodeQL flags (a key like
  // "constructor" or "__proto__" would address the prototype chain rather than
  // a conversation). A Map has no prototype chain to address, so the class is
  // structurally impossible rather than guarded against.
  queuedInputs: ReadonlyMap<string, QueuedInput[]>;
  // Returns the fresh snapshot (null when the fetch failed — "unknown", not
  // "empty"), so callers can decide without re-reading React state.
  refreshQueue: (convId: string) => Promise<QueuedInput[] | null>;
  removeQueuedInput: (convId: string, inputId: string) => Promise<void>;
  sendNowQueuedInput: (convId: string, inputId: string) => Promise<void>;
  // Follow the server-side drain of this conversation's queue to the screen
  // (#785). Call it whenever a turn's stream ends.
  followQueueDrain: (convId: string) => Promise<void>;
}

export function useTurnStream(deps: TurnStreamDeps): UseTurnStream {
  // Destructure into the SAME local names the moved bodies already use, so
  // the bodies below are verbatim. Fresh each render; no memoization.
  const {
    setConvMessages,
    getConvMessages,
    renameConvKey,
    patchAssistantMessage,
    startThinkingCrossfade,
    refreshConversations,
    loadConversation,
    loadMemories,
    loadRankedModels,
    loadCatalogModels,
    nextPendingKey,
    isPendingKey,
    setPromptForKey,
    setPendingAttachmentsForKey,
    setAttachmentErrorForKey,
    markConvUploading,
    markConvUploadDone,
    getPendingAttachmentsForKey,
    promoteComposerKey,
    setMessagesByConv,
    setConversations,
    setActiveConversationId,
    setSelectedPersona,
    setSelectedModel,
    setModelPickerOpen,
    setModelSearchQuery,
    setPendingLockdown,
    setSidebarOpen,
    setSpreadsheetNudgeDismissed,
    activeConversationIdRef,
    messagesByConvRef,
    pendingApprovalScrollRef,
    selectedModel,
    selectedPersona,
    mcpServers,
    pendingLockdown,
    userEmail,
    modelError,
    markConvStreaming,
    markConvIdle,
    abortControllersRef,
    attachedConvIdsRef,
    lastEventIdByConvRef,
    currentTurnIdByConvRef,
    reattachInFlightRef,
    streamPulseRef,
    serverHeartbeatMsRef,
    supersededStreamsRef,
    livenessInFlightRef,
    promoteStreamKey,
    streamingConvsRef,
    isStreaming,
  } = deps;

  // #785: per-conversation pending-input queue, fed by queue.updated events
  // on the live stream and by GET /queue on submit/reconnect.
  const [queuedInputs, setQueuedInputs] = useState<ReadonlyMap<string, QueuedInput[]>>(
    () => new Map<string, QueuedInput[]>(),
  );
  // One drain-follower per conversation (followQueueDrain re-enters itself
  // through the reattach it awaits).
  const queueFollowInFlightRef = useRef<Set<string>>(new Set<string>());
  const refreshQueue = async (convId: string): Promise<QueuedInput[] | null> => {
    try {
      const res = await fetch(`/api/conversations/${encodeURIComponent(convId)}/queue`);
      if (!res.ok) return null;
      const body = (await res.json()) as { items?: QueuedInput[] };
      const items = body.items ?? [];
      setQueuedInputs((cur) => new Map(cur).set(convId, items));
      return items;
    } catch {
      // snapshot refresh is best-effort; the next queue.updated self-heals
      return null;
    }
  };
  const removeQueuedInput = async (convId: string, inputId: string) => {
    try {
      await fetch(
        `/api/conversations/${encodeURIComponent(convId)}/queue/${encodeURIComponent(inputId)}`,
        { method: "DELETE" },
      );
    } finally {
      void refreshQueue(convId);
    }
  };
  const sendNowQueuedInput = async (convId: string, inputId: string) => {
    try {
      await fetch(
        `/api/conversations/${encodeURIComponent(convId)}/queue/${encodeURIComponent(inputId)}/send-now`,
        { method: "POST" },
      );
    } finally {
      void refreshQueue(convId);
    }
  };

  // ── Losing the socket is not the same as losing the turn ────────────────
  //
  // Every path below used to settle an orphaned assistant slot by stamping
  // `state: "done"` on the spot. That invents a terminal state we never
  // observed: the bubble renders as "The assistant finished without a
  // written reply." (or the literal "No response returned.") even though the
  // turn completed fine and Postgres holds the entire answer. That is the
  // walk-away-and-come-back report — a phone locks mid-turn, the OS severs
  // the socket, the user reopens the tab and is told the assistant said
  // nothing, and a manual refresh immediately shows the full reply.
  //
  // The fix is to make "we lost the stream" mean "ask the server", not
  // "declare the turn empty". Postgres is the canonical record; refreshing
  // works precisely because it reads from there. These helpers do the same
  // read automatically.

  // reconcileFromPersisted adopts the canonical transcript when it already
  // answers the turn we are holding open — programmatically what the user's
  // manual refresh does. Returns true when the swap happened.
  const reconcileFromPersisted = async (convId: string): Promise<boolean> => {
    if (isPendingKey(convId)) return false;
    try {
      const res = await fetch(`/api/conversations/${encodeURIComponent(convId)}`, {
        cache: "no-store",
      });
      if (!res.ok) return false;
      const data = (await res.json()) as { history?: HistoryEntry[] | null };
      const local = messagesByConvRef.current[convId] ?? [];
      if (!persistedAnswersLocalTurn(data.history, local)) return false;
      // Release the attach handle first: loadConversation deliberately
      // short-circuits for a conversation it believes is still streaming
      // (the in-memory copy is newer than the DB in that case). Here the
      // opposite is true — the DB is the newer copy.
      attachedConvIdsRef.current.delete(convId);
      await loadConversation(convId, {
        preserveScroll: true,
        background: true,
        restore: true,
      });
      return true;
    } catch {
      // Best-effort: the caller falls back to an honest failure marker.
      return false;
    }
  };

  // settleStreamedSlot finalizes the assistant slot a drained/severed stream
  // was writing to. Two slots need help:
  //   - one still mid-flight (`thinking`/`streaming`): the socket ended
  //     without a terminal event, so we never learned the outcome; and
  //   - one already `done` but empty after a replay GAP (see the `reconnect`
  //     handler): the terminal event arrived, the answer did not.
  // Both ask Postgres first. Only when the DB has nothing better do we settle
  // the slot ourselves, and then honestly — a retryable dropped-connection
  // notice, never a silent empty success.
  const settleStreamedSlot = async (
    convId: string,
    assistantId: number,
    gap: boolean,
  ): Promise<void> => {
    const slot = (messagesByConvRef.current[convId] ?? []).find((m) => m.id === assistantId);
    if (!slot) return;
    const midFlight = slot.state === "thinking" || slot.state === "streaming";
    const emptyAfterGap =
      gap &&
      slot.state === "done" &&
      !slot.cancelled &&
      !slot.failed &&
      !slot.modelRequired &&
      !slot.content.trim() &&
      !(slot.toolCalls && slot.toolCalls.length > 0);
    if (!midFlight && !emptyAfterGap) return;
    if (await reconcileFromPersisted(convId)) return;
    if (!midFlight) return;
    // A slot holding a pending approval or memory proposal is waiting on the
    // USER, not on the network: resolving the card resumes the turn. Settle
    // it quietly — a "Turn failed / Retry" banner over a live action card
    // would tell the reader to throw away the very decision we're asking for.
    // (ChatTranscript already suppresses the empty-reply notice here.)
    const awaitingUser =
      (slot.approvals ?? []).some((a) => a.status === "pending") ||
      (slot.memoryProposals ?? []).some((mp) => mp.status === "pending");
    if (awaitingUser) {
      patchAssistantMessage(convId, assistantId, (m) =>
        m.state === "thinking" || m.state === "streaming" ? { ...m, state: "done" } : m,
      );
      return;
    }
    patchAssistantMessage(convId, assistantId, (m) =>
      m.state === "thinking" || m.state === "streaming"
        ? {
            ...m,
            // Keep whatever partial answer did arrive; say plainly that the
            // rest was lost, and offer Retry. Anything already streamed is
            // more useful to the reader than a blanket error string.
            content: m.content || "The connection dropped before the response finished.",
            state: "done",
            failed: true,
          }
        : m,
    );
  };

  const applyStreamEvent = async (
    event: ServerEvent,
    payload: unknown,
    ctx: {
      target: string;
      assistantId: number;
      thinkingStartedAt: number;
      hasStartedStreaming: boolean;
      isReattach: boolean;
      sawTerminal: boolean;
      evicted: boolean;
      gap: boolean;
    },
  ) => {
    if (event.event === "reconnect") {
      // Synthetic server frame. type:"evicted" means our subscription was
      // dropped for falling behind while the turn kept running — the stream
      // is about to end with a clean EOF that must NOT be read as
      // turn-complete; the pump's caller reattaches instead.
      //
      // A `missed_events` count means the server's sliding window dropped
      // events we never saw (a long, chatty turn can outrun the per-turn
      // byte cap). Record it: a turn that then completes with an empty
      // assistant slot has NOT necessarily produced no answer — we just
      // didn't receive it — so the finalizers below reconcile against
      // Postgres instead of stamping "No response returned."
      const p = payload as { type?: string; missed_events?: number };
      if (p.type === "evicted") ctx.evicted = true;
      if ((p.missed_events ?? 0) > 0) ctx.gap = true;
      return;
    }

    if (event.event === "conversation") {
      const p = payload as { id: string; title: string; persona: string; model?: string };
      // oldTarget is the per-submission pending key this turn was
      // launched with (e.g. "__pending__:1"). It's distinct from the
      // PENDING_CONV_KEY singleton — the singleton stays reserved for
      // the empty new-chat view's composer state, and every brand-new
      // submission gets its own unique pending key from nextPendingKey().
      const oldTarget = ctx.target;
      if (isPendingKey(oldTarget) && oldTarget !== p.id) {
        renameConvKey(oldTarget, p.id);
        ctx.target = p.id;
        // Migrate every pending-keyed handle onto the real conv id so
        // subsequent reads (Stop button, attached-set membership, the
        // streaming-set membership the sidebar reads) and the per-conv
        // composer draft all point at the same slot the SSE events are now
        // writing to. Both promote* helpers mutate synchronously and run
        // back-to-back, so JS single-threadedness guarantees no SSE event
        // can observe a half-renamed state between the two families. The
        // stream rename runs first, matching the prior inline ordering.
        promoteStreamKey(oldTarget, p.id);
        promoteComposerKey(oldTarget, p.id);
        // The pending lockdown flag has been promoted onto the real
        // conversation row by the backend; clear the local flag so a
        // subsequent "+ New chat" doesn't accidentally re-flag.
        setPendingLockdown(false);
      }
      const currentActive = activeConversationIdRef.current;
      // Two cases land on the active view: the user is already on this
      // conv (e.g. a sidebar-driven reattach) or the user is on the
      // per-submission pending slot that just got promoted to a real
      // id. We deliberately do NOT auto-switch when currentActive is
      // null — that would yank a user back to a chat they've explicitly
      // navigated away from (submit → click "+ New chat" race).
      // submitPrompt sets active = pending key synchronously before the
      // POST, so by the time this event lands the user's slot is either
      // still pk (match the second branch) or they moved on (don't
      // touch their view).
      if (currentActive === p.id || currentActive === oldTarget) {
        activeConversationIdRef.current = p.id;
        setActiveConversationId(p.id);
        setSelectedPersona(p.persona);
        if (typeof p.model === "string") setSelectedModel(p.model || currentDefaultModel());
      }
      // Optimistically insert the row into the sidebar list so the
      // streaming dot can render *during* the turn rather than racing
      // refreshConversations(). The async refresh below still runs and
      // fills in any fields the conv event didn't carry (lockdown
      // status, accurate updated_at). Without this insert the sidebar
      // row only appeared after the async fetch came back — and on a
      // fast mock turn that often landed after turn.completed, so the
      // dot never painted.
      setConversations((curr) => {
        if (curr.some((c) => c.id === p.id)) return curr;
        const optimistic: ConversationSummary = {
          id: p.id,
          title: p.title,
          persona: p.persona,
          model: typeof p.model === "string" ? p.model : "",
          pinned: false,
          updated_at: Math.floor(Date.now() / 1000),
        };
        return [optimistic, ...curr];
      });
      void refreshConversations();
      return;
    }

    if (event.event === "user.message") {
      // Replay-only event from chat-server's per-turn buffer (see
      // server.go:postChat). On the live POST, the user message slot
      // was already created locally in submitMessage; this handler
      // is a no-op then. On a refresh-mid-turn, the local cache was
      // wiped and Postgres doesn't have the user message yet
      // (AppendHistory only fires after RunTurn completes), so reattach
      // would otherwise show a stranded "Thinking…" with no question
      // above it. Insert the user slot if it's missing — keyed on
      // adjacency to the assistant slot so we don't double-up.
      const p = payload as { text?: string; steered?: boolean };
      const text = (p.text ?? "").trim();
      if (!text) return;
      if (p.steered) {
        // A steered mid-turn input accepted at a step boundary (#785): render
        // its user bubble above the streaming assistant slot. This must be
        // handled HERE, before the replay dedup below — the adjacency check
        // would see the turn's ORIGINAL user message directly above the
        // assistant slot and silently drop the steered bubble (which is
        // exactly what happened when this case lived in a second, unreachable
        // user.message branch further down).
        setConvMessages(ctx.target, (current) => {
          const next = current.slice();
          const aIdx = next.findIndex((m) => m.id === ctx.assistantId);
          const prev = aIdx > 0 ? next[aIdx - 1] : null;
          // Replay dedup: on a reattach that kept local state, the bubble is
          // already there.
          if (prev && prev.role === "user" && prev.content === text) return current;
          const bubble = {
            id: nowMs(),
            role: "user" as const,
            content: text,
            state: "done" as const,
          };
          if (aIdx >= 0) next.splice(aIdx, 0, bubble);
          else next.push(bubble);
          return next;
        });
        return;
      }
      setConvMessages(ctx.target, (current) => {
        const assistantIdx = current.findIndex((m) => m.id === ctx.assistantId);
        if (assistantIdx < 0) return current;
        const prev = assistantIdx > 0 ? current[assistantIdx - 1] : null;
        if (prev && prev.role === "user" && prev.content === text) return current;
        if (prev && prev.role === "user") return current; // already a user msg, leave it (could be edited text)
        const userMsg: Message = {
          id: ctx.assistantId - 1,
          role: "user",
          content: text,
          state: "done",
        };
        const next = current.slice();
        next.splice(assistantIdx, 0, userMsg);
        return next;
      });
      return;
    }

    if (event.event === "reasoning.start" || event.event === "reasoning.delta") {
      const p = payload as { text?: string };
      if (!p.text) return;
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        reasoning: (m.reasoning ?? "") + p.text,
      }));
      return;
    }

    if (event.event === "reasoning.end") {
      return;
    }

    if (event.event === "fleet.context_pressure") {
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) =>
        applyContextPressure(m, payload as ContextPressureEventPayload),
      );
      return;
    }

    if (event.event === "fleet.context_compacted") {
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) =>
        applyContextCompacted(m, payload as ContextCompactedEventPayload),
      );
      return;
    }

    if (event.event === "text.delta") {
      const p = payload as { text?: string };
      if (!p.text) return;

      // Honor the minimum-thinking delay only on the initial POST path.
      // On reattach the turn is already well underway, so holding back
      // tokens would just add perceived latency on top of the reconnect.
      if (!ctx.isReattach) {
        const elapsed = nowMs() - ctx.thinkingStartedAt;
        if (elapsed < minimumThinkingMs) {
          await new Promise((resolve) =>
            window.setTimeout(resolve, minimumThinkingMs - elapsed),
          );
        }
      }
      if (!ctx.hasStartedStreaming) {
        ctx.hasStartedStreaming = true;
        startThinkingCrossfade(ctx.assistantId);
      }
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        content: m.content + p.text,
        state: "streaming",
      }));
      return;
    }

    if (event.event === "tool.call") {
      const p = payload as { id: string; name: string; input: string };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        toolCalls: [
          ...(m.toolCalls ?? []),
          { id: p.id, name: p.name, input: p.input, state: "pending" },
        ],
      }));
      return;
    }

    if (event.event === "subagent.progress") {
      // Live sub-agent activity (#1043 follow-up). Attaches to the spawn chip
      // by tool_call_id — a turn can fan out several children concurrently, so
      // the child session id alone would not place the event. An event whose
      // call we haven't seen (reattach mid-child) is dropped rather than
      // inventing a chip.
      const p = payload as SubagentProgressEventPayload;
      if (!p.tool_call_id) return;
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        toolCalls: (m.toolCalls ?? []).map((tc) =>
          tc.id === p.tool_call_id
            ? { ...tc, subagent: applySubagentProgress(tc.subagent, p) }
            : tc,
        ),
      }));
      return;
    }

    if (event.event === "turn.retry") {
      // Non-terminal: fantasy's inner retry is backing off after a
      // transient provider failure (429 / 5xx / etc). Surface a small
      // inline badge so the user knows we're waiting, not stuck.
      // clearRetryNotice is called on the next forward-progress event
      // (text.delta / tool.call) or when a terminal event supersedes.
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) =>
        applyRetryNotice(m, payload as RetryEventPayload),
      );
      return;
    }

    if (event.event === "turn.model_required") {
      // Terminal: the server gave up on the current model and wants the
      // user to pick a different one. We mark the turn done+failed (so the
      // composer unlocks) and stash the server's reason + copy on the
      // message for the inline "pick another model" banner. We also
      // auto-open the model picker so the user doesn't have to hunt for
      // it — the picker is dismissible with Escape.
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) =>
        applyModelRequired(m, payload as ModelRequiredEventPayload),
      );
      // Only auto-open the picker when the affected conversation is the
      // one currently on screen; otherwise the user just switched tabs
      // and a surprise dropdown in the new view would be jarring.
      if (ctx.target === activeConversationIdRef.current) {
        setModelPickerOpen(true);
        setModelSearchQuery("");
        void loadRankedModels();
        void loadCatalogModels();
      }
      return;
    }

    if (event.event === "tool.result") {
      const p = payload as { id: string; name: string; text: string; is_err: boolean };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => {
        const toolCalls = (m.toolCalls ?? []).map((tc) =>
          tc.id === p.id ? { ...tc, resultText: p.text, state: (p.is_err ? "error" : "done") as ToolCallState } : tc,
        );
        let pythonStreams = m.pythonStreams;
        if (p.name === "run_python" && p.text) {
          pythonStreams = [...(m.pythonStreams ?? []), parsePythonStream(p.text)];
        }
        return { ...clearRetryNotice(m), toolCalls, pythonStreams };
      });
      return;
    }

    if (event.event === "conversation.title_updated") {
      const p = payload as { id: string; title: string };
      setConversations((curr) =>
        curr.map((c) => (c.id === p.id ? { ...c, title: p.title } : c)),
      );
      return;
    }

    if (event.event === "tool.approval_required") {
      const p = payload as {
        approval_id: string;
        tool: string;
        summary: Approval["summary"];
        expires_at?: number;
        mcp_server?: string;
        mcp_account?: string;
      };
      // send_email cards can land below an expanded preview iframe — queue
      // a scroll-into-view so the user sees the action card without
      // hunting for it. Bash/preview cards stay quiet (preview is always
      // attention-grabbing on its own; bash typically already has focus).
      const isSendApproval = p.tool === "send_email" || p.tool.endsWith("_send_email");
      if (isSendApproval) pendingApprovalScrollRef.current = p.approval_id;
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...m,
        approvals: [
          ...(m.approvals ?? []),
          {
            id: p.approval_id,
            tool: p.tool,
            summary: p.summary,
            status: "pending",
            expiresAt: p.expires_at,
            mcpServer: p.mcp_server,
            mcpAccount: p.mcp_account,
          },
        ],
      }));
      return;
    }

    // A notify-mode critical tool (#1153) already RAN. The card is a record, not
    // a question: it lands already approved, carries the bundle-authored undo
    // line as its result, and has no countdown to miss — which was the point.
    if (event.event === "tool.action_recorded") {
      const p = payload as {
        approval_id: string;
        tool: string;
        summary: Approval["summary"];
        undo_hint?: string;
        result_text?: string;
        mcp_server?: string;
        mcp_account?: string;
      };
      // Prefer the server's persisted record text so the live card reads
      // byte-identically to the reloaded one; the undo-hint synthesis stays
      // as the fallback for a server one release behind this client.
      const fallback = (p.undo_hint ?? "").trim()
        ? `Ran without asking. ${(p.undo_hint ?? "").trim()}`
        : "Ran without asking.";
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...m,
        approvals: [
          ...(m.approvals ?? []),
          {
            id: p.approval_id,
            tool: p.tool,
            summary: p.summary,
            status: "approved" as ApprovalStatus,
            resultText: (p.result_text ?? "").trim() || fallback,
            recorded: true,
            mcpServer: p.mcp_server,
            mcpAccount: p.mcp_account,
          },
        ],
      }));
      return;
    }

    if (event.event === "tool.approval_superseded") {
      const p = payload as { tool: string };
      setMessagesByConv((prev) => {
        const existing = prev[ctx.target];
        if (!existing) return prev;
        const next = existing.map((msg) => {
          if (!msg.approvals?.length) return msg;
          const touched = msg.approvals.map((ap) =>
            ap.tool === p.tool && ap.status === "pending"
              ? { ...ap, status: "rejected" as ApprovalStatus, resultText: "Superseded by a newer call." }
              : ap,
          );
          return { ...msg, approvals: touched };
        });
        return { ...prev, [ctx.target]: next };
      });
      return;
    }

    if (event.event === "memory.proposed") {
      const p = payload as {
        proposal_id: string;
        content: string;
        kind?: string;
        supersedes_content?: string;
      };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => {
        // Idempotent against the re-hydrated proposal a focus-event
        // loadConversation may have just dropped on this same message.
        const existing = m.memoryProposals ?? [];
        if (existing.some((mp) => mp.id === p.proposal_id)) {
          return m;
        }
        return {
          ...m,
          memoryProposals: [
            ...existing,
            {
              id: p.proposal_id,
              content: p.content,
              kind: p.kind,
              supersedesContent: p.supersedes_content,
              status: "pending",
            },
          ],
        };
      });
      return;
    }

    if (event.event === "turn.error") {
      ctx.sawTerminal = true;
      const p = payload as { message?: string };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        content: m.content || p.message || "Something went wrong.",
        state: "done",
        failed: true,
      }));
      return;
    }

    if (event.event === "turn.cancelled") {
      ctx.sawTerminal = true;
      const p = payload as {
        cost_usd?: number;
        prompt_tokens?: number;
        prompt_tokens_last_step?: number;
        completion_tokens?: number;
        cached_tokens?: number;
        cache_creation_tokens?: number;
        duration_ms?: number;
        model?: string;
      };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        state: "done",
        cancelled: true,
        summary: {
          costUsd: p.cost_usd ?? 0,
          promptTokens: p.prompt_tokens ?? 0,
          promptTokensLastStep: p.prompt_tokens_last_step,
          completionTokens: p.completion_tokens ?? 0,
          cachedTokens: p.cached_tokens ?? 0,
          cacheCreationTokens: p.cache_creation_tokens ?? 0,
          durationMs: p.duration_ms ?? 0,
          cancelled: true,
          model: p.model,
        },
      }));
      return;
    }

    if (event.event === "queue.updated") {
      // Full snapshot on every queue mutation (#785) — no event sourcing.
      const p = payload as { items?: QueuedInput[] };
      setQueuedInputs((cur) => new Map(cur).set(ctx.target, p.items ?? []));
      return;
    }

    if (event.event === "history.persisted") {
      // Post-persist id notification (server.go:runTurnAsync): the turn's
      // history rows now exist in Postgres, and this carries their {id, role}
      // pairs in insert order. Backfill Message.dbId on the just-streamed
      // user + assistant messages so persisted-only affordances (the Branch
      // button, #454) appear immediately instead of after a reload. Mirrors
      // historyToMessages: a message's dbId is the MAX entry id it spans.
      const p = payload as { entries?: Array<{ id?: number; role?: string }> };
      let userMax = 0;
      let assistantMax = 0;
      for (const e of p.entries ?? []) {
        const id = typeof e.id === "number" ? e.id : 0;
        if (!id) continue;
        if (e.role === "user") userMax = Math.max(userMax, id);
        else if (e.role === "assistant") assistantMax = Math.max(assistantMax, id);
      }
      if (!userMax && !assistantMax) return;
      setConvMessages(ctx.target, (current) => {
        const next = current.slice();
        const aIdx = next.findIndex((m) => m.id === ctx.assistantId);
        if (aIdx >= 0 && assistantMax) {
          next[aIdx] = { ...next[aIdx], dbId: Math.max(next[aIdx].dbId ?? 0, assistantMax) };
        }
        // The user message sits directly above its assistant slot; only fill a
        // missing dbId (an edited/branched historical message keeps its own).
        const uIdx = aIdx - 1;
        if (userMax && uIdx >= 0 && next[uIdx].role === "user" && !next[uIdx].dbId) {
          next[uIdx] = { ...next[uIdx], dbId: userMax };
        }
        return next;
      });
      return;
    }

    if (event.event === "turn.completed") {
      ctx.sawTerminal = true;
      const p = payload as {
        cost_usd?: number;
        prompt_tokens?: number;
        prompt_tokens_last_step?: number;
        completion_tokens?: number;
        cached_tokens?: number;
        cache_creation_tokens?: number;
        duration_ms?: number;
        model?: string;
      };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => ({
        ...clearRetryNotice(m),
        // ctx.gap: the replay skipped events we never received, so an empty
        // slot means "we missed the answer", not "there was no answer".
        // Leave it empty and let settleStreamedSlot pull the real one from
        // Postgres once the stream has drained.
        content: m.content || (m.reasoning || ctx.gap ? "" : "No response returned."),
        state: "done",
        summary: {
          costUsd: p.cost_usd ?? 0,
          promptTokens: p.prompt_tokens ?? 0,
          promptTokensLastStep: p.prompt_tokens_last_step,
          completionTokens: p.completion_tokens ?? 0,
          cachedTokens: p.cached_tokens ?? 0,
          cacheCreationTokens: p.cache_creation_tokens ?? 0,
          durationMs: p.duration_ms ?? 0,
          model: p.model,
        },
      }));
      return;
    }
  };

  const pumpStreamResponse = async (
    response: Response,
    ctx: {
      target: string;
      assistantId: number;
      thinkingStartedAt: number;
      hasStartedStreaming: boolean;
      isReattach: boolean;
      sawTerminal: boolean;
      evicted: boolean;
      gap: boolean;
    },
  ) => {
    if (!response.body) {
      throw new Error("Empty response body from chat-server.");
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    // The cadence this stream promises. An attached stream writes at least one
    // byte per interval (a real event resets the timer, otherwise a `:
    // keepalive` comment fires), which is what lets checkStreamLiveness treat
    // prolonged silence as proof of death rather than merely as grounds for
    // suspicion. Absent or 0 → keepalives are off and silence proves nothing.
    const advertisedHeartbeat = Number(
      response.headers.get("X-Fleet-Heartbeat-Interval-Ms") ?? "",
    );
    serverHeartbeatMsRef.current = Number.isFinite(advertisedHeartbeat)
      ? Math.max(0, advertisedHeartbeat)
      : 0;

    // Liveness pulse. Every byte off the socket counts — including a heartbeat
    // comment, which carries no event but does prove the connection is alive.
    // Seeded at attach so silence is measured from when this stream started,
    // not from the epoch.
    const beat = () => {
      const prev = streamPulseRef.current[ctx.target];
      streamPulseRef.current[ctx.target] = {
        at: nowMs(),
        seq: (prev?.seq ?? 0) + 1,
      };
    };
    beat();

    const readChunk = async () =>
      await new Promise<ReadableStreamReadResult<Uint8Array>>((resolve, reject) => {
        let timeoutId: number | null = null;
        let settled = false;

        const cleanup = () => {
          settled = true;
          if (timeoutId !== null) window.clearTimeout(timeoutId);
          document.removeEventListener("visibilitychange", handleVisibilityChange);
        };
        const rejectIdle = () => {
          cleanup();
          void reader.cancel("idle timeout");
          reject(new Error("The chat server stopped responding."));
        };
        const armTimeout = () => {
          if (settled) return;
          if (timeoutId !== null) window.clearTimeout(timeoutId);
          timeoutId = window.setTimeout(() => {
            if (document.visibilityState !== "visible") {
              timeoutId = null;
              return;
            }
            rejectIdle();
          }, streamIdleTimeoutMs);
        };
        const handleVisibilityChange = () => {
          if (document.visibilityState === "visible") armTimeout();
        };

        document.addEventListener("visibilitychange", handleVisibilityChange);
        if (document.visibilityState === "visible") {
          armTimeout();
        }
        void reader.read().then(
          (result) => {
            cleanup();
            resolve(result);
          },
          (err: unknown) => {
            cleanup();
            reject(err);
          },
        );
      });

    while (true) {
      const { done, value: chunk } = await readChunk();
      if (done) break;
      beat();
      buffer += decoder.decode(chunk, { stream: true });
      const parsed = parseSseChunk(buffer);
      buffer = parsed.remainder;

      for (const event of parsed.events) {
        let payload: unknown = {};
        try {
          payload = JSON.parse(event.data);
        } catch {
          continue;
        }

        // Turn-boundary reset + monotonic id dedup. SSE event IDs are
        // monotonic WITHIN a turn but reset to 1 for each new turn, so a
        // `turn.started` with a new turn_id clears the idempotency guard
        // (otherwise the fresh turn's id=1 is dropped against the prior turn's
        // final id), and any already-applied id is dropped (the reattach replay
        // overlap). This is the pure stepStreamDedup reducer (tested in
        // sse.test.ts); the two ref maps persist its per-conv state.
        const prev = {
          lastEventId: lastEventIdByConvRef.current[ctx.target] ?? 0,
          currentTurnId: currentTurnIdByConvRef.current[ctx.target],
        };
        const { state, drop } = stepStreamDedup(prev, event, payload);
        if (state.currentTurnId !== undefined) {
          currentTurnIdByConvRef.current[ctx.target] = state.currentTurnId;
        }
        lastEventIdByConvRef.current[ctx.target] = state.lastEventId;
        if (drop) continue;

        await applyStreamEvent(event, payload, ctx);
      }
    }
  };

  // Resolves true when this call took ownership of the conversation and pumped
  // a stream — the signal followQueueDrain uses to tell "we showed that turn"
  // apart from "there was nothing to attach to (yet)".
  const reattachToConv = async (convId: string): Promise<boolean> => {
    if (attachedConvIdsRef.current.has(convId)) return false;
    if (reattachInFlightRef.current.has(convId)) return false;
    reattachInFlightRef.current.add(convId);
    // Hoisted so the outer catch can ask the same "was this socket superseded
    // on purpose?" question the inner finally does.
    let abortController: AbortController | null = null;
    try {
      const probe = await fetch(`/api/conversations/${convId}/inflight`, { cache: "no-store" });
      if (!probe.ok) return false;
      const info = (await probe.json()) as {
        inflight?: boolean;
        turn_id?: string;
        last_event_id?: number;
      };
      // Reattach in two cases:
      //   - inflight=true: turn still generating, attach for live tokens.
      //   - inflight=false + turn_id present: turn finished within the
      //     retain window (server.go:bufferRetainTTL). The buffer holds
      //     events the SSE missed when the socket got severed at lock
      //     time, including turn.finished. Replaying drains them and
      //     transitions the slot to "done" — exactly what the
      //     phone-lock-mid-turn flow needs. Without this, the catch
      //     branch in streamTurn paints "Turn failed" even though the
      //     server actually finished cleanly.
      if (!info.inflight && !info.turn_id) return false;
      if (attachedConvIdsRef.current.has(convId)) return false;

      // Nothing left to put on screen: the conversation already ends in a
      // FINISHED assistant turn, so replaying would duplicate every event onto
      // a fresh slot at the end of the conversation. Two ways we get here, and
      // both must decline.
      //
      //   - The server says the turn is finished and is only holding its
      //     retain buffer. That reattach (PR #94) exists for the
      //     *missing-events* case: phone locked mid-stream, SSE dropped,
      //     browser missed turn.completed, AppendHistory hadn't landed yet.
      //     Once loadConversation pulled the persisted shape, it is redundant.
      //
      //   - The server still says INFLIGHT, but for the very turn we have
      //     already streamed to completion. followQueueDrain re-reads the
      //     queue milliseconds after a turn ends, and both that snapshot and
      //     this probe lag behind it, so the follower gets sent back at a turn
      //     that is already on screen. Attaching appends a "thinking" slot
      //     below (no live slot to reuse — the last message is done), and
      //     every replayed event is then dropped by the Last-Event-ID dedup
      //     because we already applied all of them. Nothing lands in that slot
      //     and no terminal event clears it: a spinner under a finished answer.
      //     settleStreamedSlot erases it on the way out only when no other
      //     attach is in flight — loadConversation short-circuits while the
      //     conversation still looks attached — so under concurrency it
      //     survives, which is why this surfaced as an intermittent orphan
      //     rather than a reliable one.
      if (info.turn_id) {
        const alreadyStreamedThisTurn =
          currentTurnIdByConvRef.current[convId] === info.turn_id &&
          (lastEventIdByConvRef.current[convId] ?? 0) > 0;
        if (!info.inflight || alreadyStreamedThisTurn) {
          const existing = messagesByConvRef.current[convId] ?? [];
          const last = existing[existing.length - 1];
          if (last && last.role === "assistant" && last.state === "done") return false;
        }
      }

      // Align the idempotency baseline with the turn we're reattaching
      // to. If the server reports a turn_id we've never seen, this
      // is a brand-new turn (e.g. page refresh mid-flight after a
      // post-restart reissue) — reset lastEventId so id=1 isn't
      // dropped. If the turn_id matches what we already tracked, keep
      // the counter so the replay picks up exactly where we left off.
      if (info.turn_id && currentTurnIdByConvRef.current[convId] !== info.turn_id) {
        currentTurnIdByConvRef.current[convId] = info.turn_id;
        lastEventIdByConvRef.current[convId] = 0;
      }

      // Find or create the assistant slot for this turn.
      const existing = messagesByConvRef.current[convId] ?? [];
      const last = existing[existing.length - 1];
      let assistantId: number;
      if (
        last &&
        last.role === "assistant" &&
        (last.state === "streaming" || last.state === "thinking")
      ) {
        assistantId = last.id;
      } else {
        assistantId = nowMs();
        setConvMessages(convId, (curr) => [
          ...curr,
          {
            id: assistantId,
            role: "assistant",
            content: "",
            state: "thinking",
          },
        ]);
      }

      attachedConvIdsRef.current.add(convId);
      markConvStreaming(convId);

      const lastSeen = lastEventIdByConvRef.current[convId] ?? 0;
      const qs = info.turn_id ? `?turn_id=${encodeURIComponent(info.turn_id)}` : "";
      // Registered like a live turn's controller so unmount cleanup closes
      // this socket too — without it every /chat visit during a long turn
      // opened another reader that outlived the tree it patched.
      abortController = new AbortController();
      abortControllersRef.current[convId] = abortController;
      const ourController = abortController;
      let response: Response;
      try {
        response = await fetch(`/api/conversations/${convId}/stream${qs}`, {
          method: "GET",
          cache: "no-store",
          headers: { "Last-Event-ID": String(lastSeen) },
          signal: ourController.signal,
        });
        if (!response.ok) {
          // A failed reattach (server restart, expired retain buffer) must
          // not strand the conversation in "streaming": that locks the
          // composer and routes every next submit down the queue path
          // against a turn that no longer exists, until a full reload.
          attachedConvIdsRef.current.delete(convId);
          markConvIdle(convId);
          delete abortControllersRef.current[convId];
          return false;
        }
      } catch (err) {
        attachedConvIdsRef.current.delete(convId);
        markConvIdle(convId);
        delete abortControllersRef.current[convId];
        throw err;
      }

      const ctx = {
        target: convId,
        assistantId,
        thinkingStartedAt: nowMs(),
        hasStartedStreaming: false,
        isReattach: true,
        sawTerminal: false,
        evicted: false,
        gap: false,
      };
      try {
        await pumpStreamResponse(response, ctx);
      } finally {
        // Belt-and-suspenders: if the slot the pump was writing to is
        // still in a mid-flight state (`thinking` or `streaming`),
        // force it to `done`. This catches the rare case where the
        // server's retain-buffer replay seals without delivering a
        // terminal event (turn.completed/cancelled/error) — without
        // this nudge the indicator hangs and the composer stays
        // disabled until the user manually reloads. Refreshing the
        // page fixed it because it reloaded from Postgres, which has
        // the canonical final state; this just makes the in-memory
        // store converge to the same shape without a reload.
        if (ctx.evicted) {
          // The server dropped this subscription for falling behind while
          // the turn kept running (event: reconnect, type: evicted). Do NOT
          // force the slot to done or mark the conversation idle — it isn't.
          // Detach and reattach with Last-Event-ID to resume the stream.
          attachedConvIdsRef.current.delete(convId);
          if (abortControllersRef.current[convId] === ourController) {
            delete abortControllersRef.current[convId];
          }
        } else if (supersededStreamsRef.current.has(ourController)) {
          // We aborted this socket ourselves because it was dead and a
          // replacement stream now owns the conversation (checkStreamLiveness).
          // Touch nothing: the attach/streaming handles and the assistant slot
          // belong to the newer stream, and settling here would tear down a
          // turn that is very much still running.
          if (abortControllersRef.current[convId] === ourController) {
            delete abortControllersRef.current[convId];
          }
        } else {
          // Release our handles BEFORE settling: settleStreamedSlot may pull
          // the canonical transcript, and loadConversation short-circuits
          // while the conversation still looks attached.
          if (attachedConvIdsRef.current.has(convId)) {
            attachedConvIdsRef.current.delete(convId);
            markConvIdle(convId);
          }
          if (abortControllersRef.current[convId] === ourController) {
            delete abortControllersRef.current[convId];
          }
          // The replay ended — cleanly, or because the socket died under a
          // locked phone. Either way the canonical record is in Postgres.
          // Reconcile against it rather than stamping a terminal state we
          // never observed; only a turn the DB has no answer for is settled
          // locally, and then as a retryable dropped connection.
          await settleStreamedSlot(convId, ctx.assistantId, ctx.gap);
          // Refresh so any server-side state we missed (new title, updated
          // metrics sidebar) shows.
          void refreshConversations();
          // This turn is over — anything queued behind it is draining
          // server-side right now, and nothing else would put it on screen.
          void followQueueDrain(convId);
        }
      }
      if (ctx.evicted) {
        // Outside the finally so the in-flight guard (cleared by our caller's
        // finally) is released before the retry fires.
        setTimeout(() => void reattachToConv(convId), 150);
      }
      return true;
    } catch {
      // Silent — reattach is best-effort. A stream we superseded on purpose is
      // not a failure and no longer owns these handles; leave them to the
      // replacement.
      if (
        !(abortController && supersededStreamsRef.current.has(abortController)) &&
        attachedConvIdsRef.current.has(convId)
      ) {
        attachedConvIdsRef.current.delete(convId);
        markConvIdle(convId);
      }
      return false;
    } finally {
      reattachInFlightRef.current.delete(convId);
    }
  };

  // ── Following the queue to the screen ──────────────────────────────────
  //
  // A submission accepted while a turn was running (#785) is durable
  // server-side and drains as its OWN turn, kicked from the finishing turn's
  // tail call. Nothing pushed that to the browser: the finishing turn's event
  // buffer is already sealed when the drain happens (so the settle-time
  // `queue.updated` lands nowhere), and the drained turn opens a brand-new
  // buffer that this client never asked to attach to. The result was the bug
  // users reported as "my queued message never sent": the composer went idle,
  // the QUEUED chip sat there forever, and the drained turn ran — invisibly —
  // to a Postgres row only a page reload revealed.
  //
  // So after every turn's stream ends, follow the queue: re-read the
  // authoritative snapshot, attach to the turn the drain started, and repeat
  // for the next row. Three outcomes, all honest:
  //   - attached → the drained turn streams like any other turn, and when it
  //     ends this runs again for whatever is behind it;
  //   - the queue emptied without us ever attaching → the row drained and
  //     finished faster than we looked (or its retain buffer went), so adopt
  //     the canonical transcript from Postgres;
  //   - the backoff ran out with the row still queued → stop, leaving an
  //     ACCURATE chip strip whose send-now button forces the drain. That is
  //     the deliberate no-auto-drain-at-boot case (docs/INPUT-QUEUE.md).
  const followQueueDrain = async (convId: string) => {
    if (isPendingKey(convId)) return; // a brand-new chat has no queue
    if (queueFollowInFlightRef.current.has(convId)) return;
    queueFollowInFlightRef.current.add(convId);
    // Set whenever we see drain work we have not yet put on screen; cleared
    // by attaching to the turn that ran it. Still set when the queue empties
    // means the turn happened where nobody could see it.
    let unseenWork = false;
    // Hard stop on how many turns one follow-through will chain. The server
    // caps a conversation at maxPendingInputs (20) queued rows, so this can
    // only bite if a reattach kept reporting success without the queue ever
    // shrinking — a loop no user action could break out of.
    let streamed = 0;
    try {
      for (let attempt = 0; ; attempt++) {
        // Another path (a fresh submission, a liveness reconnect) owns the
        // conversation now; its own stream end re-enters here.
        if (attachedConvIdsRef.current.has(convId)) return;
        const items = await refreshQueue(convId);
        if (items === null) return; // snapshot unknown — don't guess
        if (!hasPendingQueueWork(items)) break;
        unseenWork = true;
        if (await reattachToConv(convId)) {
          // That turn is on screen and finished. Look again from the top:
          // the next queued row gets the same treatment.
          unseenWork = false;
          streamed += 1;
          if (streamed >= 20) return;
          attempt = -1;
          continue;
        }
        const delay = queueDrainFollowDelaysMs[attempt];
        if (delay === undefined) return;
        await new Promise((resolve) => window.setTimeout(resolve, delay));
      }
      if (unseenWork) {
        await loadConversation(convId, { background: true });
      }
    } finally {
      queueFollowInFlightRef.current.delete(convId);
    }
  };

  // ── Telling a DEAD socket apart from a QUIET one ────────────────────────
  //
  // A phone that locks mid-turn leaves a ZOMBIE socket: the fetch reader
  // neither delivers another chunk nor rejects, so the conversation goes on
  // looking attached long after its connection is gone. Every other recovery
  // path bails out on "already attached", so nothing notices until the
  // multi-minute idle timeout fires.
  //
  // Two different things can be true behind that silence, and they need
  // opposite responses:
  //   - the turn FINISHED while we were away → adopt the persisted transcript
  //     (reconcileFromPersisted); the answer is already in Postgres.
  //   - the turn is STILL GENERATING → there is nothing to adopt yet. Replace
  //     the dead socket and resume the live stream from our last applied
  //     event id, so tokens start landing again instead of the user watching
  //     a thinking indicator that will never move.
  //
  // The hard part is proving deadness without false-positiving a healthy but
  // quiet stream. The proof used here is two-part and does not depend on the
  // (operator-configurable) heartbeat: the server reports it has emitted PAST
  // our last applied event id, and our socket then produces no bytes at all
  // during a grace window. A frozen-but-alive socket delivers its buffered
  // bytes as soon as the page thaws, well inside that window; a severed one
  // delivers nothing, ever.
  //
  // A false positive is cheap by construction — the replacement attaches with
  // Last-Event-ID set to what we already applied, the server replays from
  // there, and stepStreamDedup drops anything we have seen. The cost is one
  // reconnect, never a duplicated or lost token.

  // supersedeStream retires the socket currently attached to convId in favor
  // of a replacement. The marker is what keeps the retiring stream's own
  // teardown from tearing down the turn (see the supersededStreamsRef checks
  // in reattachToConv and submitPrompt): without it the abort would land as a
  // user Stop, or settle a still-running turn as a dropped connection.
  // Resolves once the old stream has unwound far enough for a reattach to be
  // able to claim the conversation.
  // retireStream marks `doomed` superseded and aborts it — but only if it is
  // still the controller registered for this conversation. The identity check
  // is what keeps a slow await earlier in the caller from retiring a stream
  // that arrived in the meantime: aborting someone else's live socket while
  // flagging it "superseded" would silently strand the turn it was reading.
  // Reports whether it actually retired anything.
  const retireStream = (convId: string, doomed: AbortController | null): boolean => {
    if (!doomed) return false;
    if (abortControllersRef.current[convId] !== doomed) return false;
    supersededStreamsRef.current.add(doomed);
    delete abortControllersRef.current[convId];
    doomed.abort();
    return true;
  };

  // supersedeStream retires the dead socket and waits until a replacement can
  // actually claim the conversation.
  const supersedeStream = async (
    convId: string,
    doomed: AbortController | null,
  ): Promise<void> => {
    retireStream(convId, doomed);
    // Hand the slot back so the replacement's own guards let it through. The
    // retiring stream will not do this for us — that is the whole point of
    // the marker.
    attachedConvIdsRef.current.delete(convId);
    // Wait for the retiring stream's in-flight guard to clear, or reattach
    // would refuse ("a reattach is already running for this conv") and we
    // would end up with no stream at all. Bounded: a wedged unwind degrades
    // to no reconnect this round, and the watchdog tries again.
    const deadline = nowMs() + supersedeUnwindTimeoutMs;
    while (reattachInFlightRef.current.has(convId) && nowMs() < deadline) {
      await delay(50);
    }
  };

  // checkStreamLiveness is the single entry point for the tab-return listener
  // and the watchdog interval. `force` skips the silence gate: an explicit
  // return to the tab is always worth a probe, whereas the periodic watchdog
  // only spends one on a stream that has actually gone quiet (a healthy
  // stream heartbeats, so it is never probed).
  //
  // Returns what it did, for tests and for the caller's logging:
  //   "idle"        — nothing attached / nothing mid-flight here
  //   "healthy"     — the socket is fine (or too fresh to suspect)
  //   "recovered"   — the turn had finished; the persisted transcript is in
  //   "reconnected" — the turn is alive; a fresh socket now owns it
  const checkStreamLiveness = async (
    convId: string,
    opts: { force?: boolean } = {},
  ): Promise<"idle" | "healthy" | "recovered" | "reconnected"> => {
    if (isPendingKey(convId)) return "idle";
    if (!attachedConvIdsRef.current.has(convId)) return "idle";
    if (livenessInFlightRef.current.has(convId)) return "idle";
    const local = messagesByConvRef.current[convId] ?? [];
    const last = local[local.length - 1];
    if (!last || last.role !== "assistant") return "idle";
    if (last.state !== "thinking" && last.state !== "streaming") return "idle";

    const heartbeatMs = serverHeartbeatMsRef.current;
    const pulseBefore = streamPulseRef.current[convId];
    const silentMs = nowMs() - (pulseBefore?.at ?? 0);
    // Bytes arrived recently — the socket is demonstrably alive, so there is
    // nothing to probe. The watchdog uses the wide gate (this is its common
    // case, and it costs nothing). A forced call still skips a stream that
    // produced bytes within the grace window it would otherwise sit through:
    // on an actively streaming conversation, `focus` fires often and every
    // one of those probes could only ever conclude "healthy".
    if (silentMs < (opts.force ? streamLivenessGraceMs : streamSilenceProbeMs(heartbeatMs))) {
      return "healthy";
    }

    // Captured before the first await: every retire below checks that this is
    // still the conversation's registered controller.
    const doomed = abortControllersRef.current[convId] ?? null;
    livenessInFlightRef.current.add(convId);
    try {
      let inflight = false;
      let serverLastEventId = 0;
      try {
        const probe = await fetch(
          `/api/conversations/${encodeURIComponent(convId)}/inflight`,
          { cache: "no-store" },
        );
        if (!probe.ok) return "healthy";
        const info = (await probe.json()) as {
          inflight?: boolean;
          last_event_id?: number;
        };
        inflight = Boolean(info?.inflight);
        serverLastEventId = info?.last_event_id ?? 0;
      } catch {
        // The probe itself failed (offline, server bounce). Assume nothing.
        return "healthy";
      }

      if (!inflight) {
        // The turn is over. Postgres is authoritative; adopt it and retire the
        // socket. reconcileFromPersisted releases the attach handle itself.
        if (!(await reconcileFromPersisted(convId))) return "healthy";
        // If something else has claimed the conversation since we started
        // (loadConversation ends by re-probing for an in-flight turn), that
        // stream owns the streaming flag — leave it be. Otherwise the turn is
        // over and the composer should be free. Note this is about a
        // DIFFERENT controller having appeared, not about `doomed` existing:
        // an attach handle with no registered controller still needs idling.
        const current = abortControllersRef.current[convId];
        const claimedByOther = Boolean(current) && current !== doomed;
        retireStream(convId, doomed);
        if (!claimedByOther) markConvIdle(convId);
        // Another turn-ends moment, reached without any stream's finally: a
        // follow-up queued behind this turn is draining server-side and needs
        // the same hand-off. Self-guards if another controller owns the conv.
        void followQueueDrain(convId);
        return "recovered";
      }

      // Still generating. Two independent kinds of evidence that our socket is
      // not carrying it:
      //
      //   - the server has emitted PAST what we applied, so events exist that
      //     we are not receiving; or
      //   - the stream has been silent for longer than the keepalive cadence
      //     it promised us. This is the one that covers a turn sitting in a
      //     long tool call: the server emits nothing to fall behind on, so the
      //     first test can never fire, but the keepalive still should have.
      //
      // Neither alone is conclusive — hence the grace window below — but
      // without the second, a socket that dies during a quiet stretch is
      // invisible until the turn resumes emitting.
      const applied = lastEventIdByConvRef.current[convId] ?? 0;
      const serverAhead = serverLastEventId > applied;
      const missedKeepalives = silentMs >= streamDeadSilenceMs(heartbeatMs);
      if (!serverAhead && !missedKeepalives) return "healthy";

      // Give the socket its chance: a frozen-but-alive connection flushes as
      // soon as the page thaws.
      await delay(streamLivenessGraceMs);
      const pulseAfter = streamPulseRef.current[convId];
      if ((pulseAfter?.seq ?? 0) !== (pulseBefore?.seq ?? 0)) return "healthy";
      // Re-check the preconditions: the grace window is long enough for the
      // turn to have ended, or for another path to have settled the slot.
      if (!attachedConvIdsRef.current.has(convId)) return "idle";
      const stillLocal = messagesByConvRef.current[convId] ?? [];
      const stillLast = stillLocal[stillLocal.length - 1];
      if (
        !stillLast ||
        stillLast.role !== "assistant" ||
        (stillLast.state !== "thinking" && stillLast.state !== "streaming")
      ) {
        return "idle";
      }

      // Dead. Retire it and resume the turn on a fresh socket; reattachToConv
      // reuses the mid-flight slot and replays from our last applied event id,
      // so the partial answer on screen is kept and nothing is rendered twice.
      await supersedeStream(convId, doomed);
      await reattachToConv(convId);
      return "reconnected";
    } finally {
      livenessInFlightRef.current.delete(convId);
    }
  };

  // sweepStreamLiveness checks EVERY conversation this client has a socket
  // attached to, not just the one on screen. Chats stream in parallel (the
  // sidebar paints a working dot per busy chat), and a background
  // conversation's socket dies exactly the same way the foreground one's does
  // — it just has nobody watching it. Snapshotted because checkStreamLiveness
  // mutates the attached set when it reconnects, and per-conversation failures
  // are contained so one bad conv cannot abort the sweep.
  //
  // Only while the tab is visible: a hidden tab's timers are throttled and its
  // sockets may be legitimately frozen, so probing then would be both
  // unreliable and pointless. The tab-return listener covers the wake-up.
  const sweepStreamLiveness = async (
    opts: { force?: boolean } = {},
  ): Promise<void> => {
    if (typeof document !== "undefined" && document.visibilityState !== "visible") return;
    const attached = Array.from(attachedConvIdsRef.current);
    if (attached.length === 0) return;
    await Promise.all(
      attached.map((convId) =>
        checkStreamLiveness(convId, opts).catch(() => "idle" as const),
      ),
    );
  };

  const streamTurn = async (
    assistantId: number,
    abortController: AbortController,
    body: Record<string, unknown>,
    initialTarget: string,
  ) => {
    const thinkingStartedAt = nowMs();
    let hasStartedStreaming = false;
    // Which conversation slot do this turn's events write to? Caller
    // (submitPrompt) picked the key: a real conv id for existing chats,
    // a per-submission pending key for brand-new chats. The conversation
    // event will rename pending → real id mid-stream. Decoupling this
    // from body.conversation_id lets two brand-new chats run in
    // parallel without colliding on a single PENDING sentinel.
    let target = initialTarget;
    attachedConvIdsRef.current.add(target);

    const response = await fetch("/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: abortController.signal,
    });

    if (!response.ok || !response.body) {
      const errorText = await response.text();
      if (response.status === 429) {
        const retry = response.headers.get("Retry-After") ?? "a moment";
        throw new Error(
          `Rate limit reached. Try again in ${retry.replace(/\D/g, "")}s.`,
        );
      }
      throw new Error(errorText || "Unable to reach the chat server.");
    }

    // The mirror image of the stale-busy race (#824): we believed the
    // conversation was IDLE, so we posted a direct submission — and the server,
    // which knew better, queued it (#785) and answered with a JSON ack instead
    // of an SSE stream. Pumping that ack as SSE is how a queued follow-up used
    // to end up as a permanent "Thinking…" bubble over a turn that was never
    // started. Drop the optimistic pair we just rendered so this submission
    // looks exactly like one made from the queue path — a chip, nothing more —
    // and let the caller's followQueueDrain put the real turn on screen when
    // the drain runs it.
    if (classifyQueueSubmitResponse(response) === "queued") {
      void response.body.cancel().catch(() => {});
      attachedConvIdsRef.current.delete(target);
      setConvMessages(target, (current) =>
        current.filter(
          (m) =>
            m.id !== assistantId &&
            // submitPrompt renders this submission's user bubble one id below.
            !(m.id === assistantId - 1 && m.role === "user"),
        ),
      );
      return;
    }

    // Fresh turn — reset the idempotency baseline for this conv so
    // the first event (id=1, usually `conversation`) isn't dropped as
    // "≤ the previous turn's final id". The turn_id arrives a frame
    // later (in turn.started) and the boundary-detection logic in
    // pumpStreamResponse keeps currentTurnIdByConvRef in sync.
    lastEventIdByConvRef.current[target] = 0;

    // Thread mutable per-turn state through the shared pump. The
    // "conversation" SSE event may rename target from PENDING_CONV_KEY
    // → a real id; pumpStreamResponse mutates ctx.target in place so
    // subsequent events in the same stream land in the right slot.
    const ctx = {
      target,
      assistantId,
      thinkingStartedAt,
      hasStartedStreaming,
      isReattach: false,
      sawTerminal: false,
      evicted: false,
      gap: false,
    };
    await pumpStreamResponse(response, ctx);
    target = ctx.target;
    hasStartedStreaming = ctx.hasStartedStreaming;

    if (!ctx.sawTerminal) {
      // The SSE body ended cleanly (reader hit EOF) WITHOUT a terminal
      // turn event (turn.completed / .error / .cancelled). On mobile
      // this is the phone-lock signature: iOS/Android close the TCP
      // socket on screen-lock while chat-server keeps generating, and
      // the closed socket surfaces here as a graceful end-of-stream
      // rather than a thrown error. Finalizing now would stamp a bogus
      // "No response returned." (or, downstream, "Turn failed") that the
      // user can only clear by refreshing. Throw instead so the catch in
      // submitPrompt runs the same probe-/inflight-then-reattach recovery
      // a refresh would — the turn is almost certainly still alive.
      throw new Error("__stream_closed_before_turn_end__");
    }

    patchAssistantMessage(target, assistantId, (m) => ({
      ...m,
      content: m.content || (m.reasoning || ctx.gap ? "" : "No response returned."),
      state: "done",
    }));
    // A replay gap (server-side sliding-window eviction on a long, chatty
    // turn) can leave the slot terminal but empty. Postgres has the answer.
    await settleStreamedSlot(target, assistantId, ctx.gap);
  };

  const regenerateLastAssistant = async () => {
    await retryLastUserMessage();
  };

  const resendUserMessage = async (userMessageId: number, editedContent: string) => {
    if (isStreaming) return;
    const trimmedContent = editedContent.trim();
    if (!trimmedContent) return;
    const targetKey = activeConversationIdRef.current ?? PENDING_CONV_KEY;
    const history = getConvMessages(targetKey);
    const idx = history.findIndex((m) => m.id === userMessageId);
    if (idx < 0 || history[idx].role !== "user") return;

    // Drop the edited user bubble and everything after it client-side;
    // submitPrompt will re-add the user bubble with the edited content.
    const trimmed = history.slice(0, idx);
    setConvMessages(targetKey, trimmed);

    const convId = activeConversationIdRef.current;
    if (convId) {
      try {
        // mode=edit_last drops the previous user turn AND its assistant
        // tail, so submitPrompt below can start fresh with the edit as the
        // current-last user message.
        await fetch(`/api/conversations/${convId}/truncate?mode=edit_last`, {
          method: "POST",
        });
      } catch {
        /* non-fatal */
      }
    }

    await submitPrompt(trimmedContent);
  };

  const retryLastUserMessage = async () => {
    if (isStreaming) return;
    const targetKey = activeConversationIdRef.current ?? PENDING_CONV_KEY;
    const history = getConvMessages(targetKey);
    let lastUser: Message | undefined;
    for (let i = history.length - 1; i >= 0; i--) {
      if (history[i].role === "user") {
        lastUser = history[i];
        break;
      }
    }
    if (!lastUser) return;

    // Drop the user bubble and everything after it client-side —
    // submitPrompt re-adds it. Keeping the bubble AND re-submitting (the
    // old behavior) left two identical user bubbles in the UI and, since
    // the default truncate keeps the latest user row server-side too,
    // persisted the prompt twice so the model was fed it twice. Mirrors
    // resendUserMessage, which is the same flow with edited content.
    const idx = history.findIndex((m) => m.id === lastUser.id);
    const trimmed = history.slice(0, idx);
    setConvMessages(targetKey, trimmed);

    const convId = activeConversationIdRef.current;
    if (convId) {
      try {
        // mode=edit_last drops the last user turn AND its assistant tail
        // server-side, so the re-submit below starts from a clean point.
        await fetch(`/api/conversations/${convId}/truncate?mode=edit_last`, {
          method: "POST",
        });
      } catch {
        // Non-fatal — the turn still works, history just contains the
        // cancelled tail (the model can handle it).
      }
    }

    await submitPrompt(lastUser.content);
  };

  const uploadPendingAttachments = async (
    composerKey: string,
  ): Promise<UploadedAttachmentMeta[]> => {
    const files = getPendingAttachmentsForKey(composerKey);
    markConvUploading(composerKey);
    setPendingAttachmentsForKey(
      composerKey,
      files.map((a) => ({ ...a, status: "uploading" as const })),
    );
    try {
      const form = new FormData();
      for (const a of files) {
        form.append("files", a.file, a.name);
      }
      const res = await fetch("/api/attachments", { method: "POST", body: form });
      if (!res.ok) {
        const text = await res.text().catch(() => res.statusText);
        throw new Error(`Attachment upload failed: ${text || res.statusText}`);
      }
      const data = (await res.json()) as { attachments?: UploadedAttachmentMeta[] };
      const attachments = data.attachments ?? [];
      if (attachments.length === 0) {
        throw new Error("Server accepted upload but returned no attachments.");
      }
      return attachments;
    } finally {
      markConvUploadDone(composerKey);
    }
  };

  const submitPrompt = async (submittedPrompt: string) => {
    const value = submittedPrompt.trim();
    // composerKey is the slot the user was typing into (real conv id or
    // the PENDING singleton for the empty new-chat view). All the
    // composer cleanup below targets THIS key so we don't blow away an
    // unrelated chat's draft if the user has navigated since clicking
    // Submit.
    const convId = activeConversationIdRef.current;
    const composerKey = convId ?? PENDING_CONV_KEY;
    if (!value || !userEmail) return;
    if (modelError) return;
    // Busy conversation (#785): the submission QUEUES server-side instead of
    // being dropped (and instead of the old implicit cancel). The composer
    // clears; the chip strip under it tracks the queued input's lifecycle.
    if (convId && streamingConvsRef.current.has(convId)) {
      setPromptForKey(composerKey, "");
      try {
        const res = await fetch("/api/chat", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            message: value,
            conversation_id: convId,
            input_id: crypto.randomUUID(),
            mode: "queue",
          }),
        });
        const kind = classifyQueueSubmitResponse(res);
        if (kind === "error") {
          setPromptForKey(composerKey, value); // give the text back
          return;
        }
        if (kind === "stream") {
          // Stale busy flag (#824): the turn we thought was running had
          // already finished server-side, so the server ignored mode:"queue"
          // and started a direct turn on THIS response. Nobody here is set
          // up to consume it — hand off to reattachToConv, which attaches
          // to the turn's buffer and replays from event 0 (the user.message
          // echo renders this submission's bubble, then tokens stream
          // normally). Cancelling this body is safe: the turn deliberately
          // outlives its originating request (server.go startTurn).
          void res.body?.cancel().catch(() => {});
          await reattachToConv(convId);
          return;
        }
      } catch {
        setPromptForKey(composerKey, value);
        return;
      }
      void refreshQueue(convId);
      return;
    }

    // Upload any pending attachments FIRST. If it fails, we bail out with
    // the text still in the composer so the user can retry without losing
    // their message. Empty list → no-op, fast path unchanged.
    let uploadedAttachments: UploadedAttachmentMeta[] = [];
    if (getPendingAttachmentsForKey(composerKey).length > 0) {
      try {
        uploadedAttachments = await uploadPendingAttachments(composerKey);
      } catch (err) {
        setAttachmentErrorForKey(
          composerKey,
          err instanceof Error ? err.message : "Upload failed.",
        );
        return;
      }
    }

    setPromptForKey(composerKey, "");
    setPendingAttachmentsForKey(composerKey, []);
    setAttachmentErrorForKey(composerKey, null);
    // Composer just emptied — re-arm the spreadsheet nudge for the next
    // upload (formerly handled by a pendingAttachments.length effect).
    setSpreadsheetNudgeDismissed(false);

    const baseId = nowMs();
    const assistantId = baseId + 1;

    // Tack a short markdown block onto the displayed user message so the
    // chips the user saw in the composer don't silently disappear — it
    // mirrors what chat-server appends server-side for the LLM.
    const displayedContent = uploadedAttachments.length > 0
      ? `${value}\n\n---\n**Attached files:**\n${uploadedAttachments
          .map((a) => `- ${a.name} (${formatBytes(a.size)})`)
          .join("\n")}`
      : value;

    const nextMessages: Message[] = [
      { id: baseId, role: "user", content: displayedContent, state: "done" },
      { id: assistantId, role: "assistant", content: "", state: "thinking" },
    ];

    // Where do this turn's stream events write to? Existing chat → its
    // slot. Brand-new chat → a per-submission pending key (NOT the
    // PENDING singleton), so subsequent "+ New chat" clicks while this
    // turn is still pre-promotion can't collide with this controller.
    // The conversation event will rename the per-submission key → the
    // real conv id when it lands.
    const initialTarget = convId ?? nextPendingKey();
    setConvMessages(initialTarget, (current) => [...current, ...nextMessages]);
    setSidebarOpen(false);

    // If this is a brand-new chat and the user is still on the empty
    // view, point the active view at the per-submission slot so the
    // messages render. If they navigated away while we were uploading
    // attachments, leave them there — the chat will land in the
    // sidebar via the optimistic insert when the conv event arrives,
    // and they can click into it from there. The ref is updated
    // synchronously so the conv-event handler (which can race the
    // React commit) sees the right "current view" value.
    if (!convId && activeConversationIdRef.current === null) {
      activeConversationIdRef.current = initialTarget;
      setActiveConversationId(initialTarget);
    }

    const abortController = new AbortController();
    abortControllersRef.current[initialTarget] = abortController;
    markConvStreaming(initialTarget);

    const trimmedModel = selectedModel.trim();
    const body: Record<string, unknown> = {
      message: value,
      persona: selectedPersona,
      model: trimmedModel,
    };
    if (uploadedAttachments.length > 0) {
      body.attachments = uploadedAttachments;
    }
    if (convId) {
      body.conversation_id = convId;
    } else {
      body.title = value.length > 80 ? value.slice(0, 80) + "…" : value;
      // Pre-chat tool toggles — the backend persists these onto the
      // new conversation so the first turn can actually use them.
      const enabledOptional = enabledOptionalMcpServerNames(mcpServers);
      if (enabledOptional.length > 0) {
        body.enabled_optional = enabledOptional;
      }
      // Seats picked before the first message (#988) — same full-map shape
      // the per-conversation POST sends, so the choice sticks.
      const mcpAccounts = mcpAccountOverrides(mcpServers);
      if (Object.keys(mcpAccounts).length > 0) {
        body.mcp_accounts = mcpAccounts;
      }
      if (pendingLockdown) {
        body.lockdown = true;
      }
    }

    // resolveTarget reverse-maps our AbortController back to whatever
    // conv-id key it lives under right now. streamTurn promotes
    // PENDING_CONV_KEY → real id mid-stream by re-keying the abort
    // controllers / attached sets / streaming set as a unit; this
    // scan is how the catch/finally below relocates "our" slot after
    // that swap. Falls back to initialTarget when no swap happened.
    const resolveTarget = (): string => {
      for (const [k, v] of Object.entries(abortControllersRef.current)) {
        if (v === abortController) return k;
      }
      return initialTarget;
    };

    try {
      await streamTurn(assistantId, abortController, body, initialTarget);
      await refreshConversations();
      void loadMemories();
    } catch (error) {
      const target = resolveTarget();
      if (supersededStreamsRef.current.has(abortController)) {
        // We aborted this POST ourselves because its socket was dead and a
        // replacement stream has taken over the conversation
        // (checkStreamLiveness). This is NOT a user Stop and NOT a failure:
        // the turn is still running and someone else is reading it now.
        // Leave the slot, the attach handle and the streaming flag alone —
        // the `finally` below makes the same check.
        return;
      }
      if (abortController.signal.aborted) {
        // User clicked Stop. Mark the turn cancelled — the server's
        // turn.cancelled event may or may not reach us before the socket
        // closes, so we set the flag defensively on the client side too.
        patchAssistantMessage(target, assistantId, (m) => ({
          ...m,
          state: "done",
          cancelled: true,
        }));
      } else {
        // Probe /inflight before declaring the turn failed. When a
        // phone backgrounds mid-stream, iOS/Android often sever the
        // TCP socket while chat-server keeps generating — flashing
        // "Turn failed" there is wrong and leaves the user unable to
        // resubmit (reattach from visibilitychange may have already
        // run with the slot still in attachedConvIdsRef, and no second
        // visibility event fires once the user is back on screen).
        //
        // Two recoverable cases — both hand off to reattachToConv,
        // which knows how to drain a finished-but-retained buffer the
        // same way it handles a still-running one:
        //   - inflight=true: live turn, attach for tokens.
        //   - inflight=false + turn_id present: turn finished while
        //     we were locked, but the buffer's still in the retain
        //     window. The replay carries turn.finished + any events
        //     the dead SSE missed; the slot lands at state="done"
        //     instead of "failed".
        let probeInflight = false;
        let probeTurnID = "";
        if (!isPendingKey(target)) {
          try {
            const probe = await fetch(`/api/conversations/${target}/inflight`, { cache: "no-store" });
            if (probe.ok) {
              const info = (await probe.json()) as { inflight?: boolean; turn_id?: string };
              probeInflight = Boolean(info?.inflight);
              probeTurnID = info?.turn_id ?? "";
            }
          } catch {
            /* probe failed — fall through to the failed marker */
          }
        }
        if (probeInflight || probeTurnID) {
          patchAssistantMessage(target, assistantId, (m) => ({
            ...m,
            state: "streaming",
          }));
          // Release the attach handle so reattachToConv can re-claim
          // it; the finally below will only reset state we still own.
          attachedConvIdsRef.current.delete(target);
          await reattachToConv(target);
          // Defensive reconcile against the probe/reattach race: if the turn
          // completed between our /inflight probe and reattach's own probe,
          // reattach short-circuits without attaching and the slot is left
          // mid-flight. Postgres has the canonical shape — pull it (and, if
          // it doesn't, leave an honest retryable marker instead of a blank
          // bubble that reads as "the assistant said nothing").
          await settleStreamedSlot(target, assistantId, false);
        } else {
          // Guard against the two-recovery-path race. When a phone
          // unlocks, the visibilitychange/focus reattach and this catch
          // can both move to settle the same slot. The reattach resumes
          // the turn and renders the full answer (state="done"), but our
          // /inflight probe only lands in this failed branch once the
          // turn has finished AND its retain buffer has been evicted —
          // by which point the slot is already a successful `done`.
          // Stamping `failed` here is the bug behind a fully-rendered
          // answer that flips to "Turn failed" a beat later. If another
          // path already finalized the turn successfully, leave it.
          const resolved = messagesByConvRef.current[target]?.find((m) => m.id === assistantId);
          if (resolved && resolved.state === "done" && !resolved.failed) {
            // Already settled successfully by another path — leave it.
          } else {
            // The probe found nothing in-flight and no retained buffer. For a
            // LONG job that finished while the phone was locked, the turn has
            // already been persisted to Postgres and its retain buffer
            // (server.go:bufferRetainTTL) has since expired — so /inflight
            // legitimately reports nothing even though the full answer exists
            // in the DB. That's the "looks failed until I refresh" report: a
            // manual refresh recovers it because it reads Postgres. Do the
            // same here BEFORE declaring failure — only stamp "failed" when
            // the DB confirms there's no completed answer for THIS turn.
            const recovered = await reconcileFromPersisted(target);
            if (!recovered) {
              // The premature-EOF sentinel is an internal signal, never a
              // user-facing string — only reachable when the turn is genuinely
              // gone (not inflight, no buffer, nothing completed in the DB).
              const rawMsg = error instanceof Error ? error.message : "Something went wrong.";
              const msg =
                rawMsg === "__stream_closed_before_turn_end__"
                  ? "The connection dropped before the response finished."
                  : rawMsg;
              // Re-check inside the patch: never downgrade a slot that reached
              // a successful terminal state between our read and this write
              // (the reattach pump runs concurrently).
              patchAssistantMessage(target, assistantId, (m) =>
                m.state === "done" && !m.failed
                  ? m
                  : {
                      ...m,
                      content: m.content || msg,
                      state: "done",
                      failed: true,
                    },
              );
            }
          }
        }
      }
      await refreshConversations();
    } finally {
      const finalTarget = resolveTarget();
      if (abortControllersRef.current[finalTarget] === abortController) {
        delete abortControllersRef.current[finalTarget];
      }
      // Superseded: a replacement stream owns this conversation's handles and
      // its assistant slot. Releasing them here would idle a composer for a
      // turn that is still generating and detach the socket that just took
      // over. Everything else below is this stream's own cleanup.
      if (!supersededStreamsRef.current.has(abortController)) {
        attachedConvIdsRef.current.delete(finalTarget);
        markConvIdle(finalTarget);
        // Last resort: if every path above missed this slot it is still
        // mid-flight, and the indicator would hang until the user refreshed.
        // Settle it — but settle it the same way the rest of the loop does,
        // by asking Postgres first. The old version stamped a bare
        // `state: "done"` here, which turned an unknown outcome into a
        // *silent empty success*: the transcript claimed the assistant
        // finished without a written reply while the DB held the full answer.
        // Any already-terminal slot (done/failed/cancelled) is left alone.
        await settleStreamedSlot(finalTarget, assistantId, false);
        // Same hand-off as the reattach path: a follow-up the user queued
        // while this turn ran drains as its own turn, and this is the only
        // moment we learn about it.
        void followQueueDrain(finalTarget);
      }
    }
  };

  return {
    reattachToConv,
    checkStreamLiveness,
    sweepStreamLiveness,
    submitPrompt,
    regenerateLastAssistant,
    resendUserMessage,
    retryLastUserMessage,
    queuedInputs,
    followQueueDrain,
    refreshQueue,
    removeQueuedInput,
    sendNowQueuedInput,
  };
}
