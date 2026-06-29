import type { Dispatch, RefObject, SetStateAction } from "react";
import {
  applyContextCompacted,
  applyContextPressure,
  applyModelRequired,
  applyRetryNotice,
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
  type ToolCallState,
} from "./history";
import { parseSseChunk, stepStreamDedup, type ServerEvent } from "@/app/lib/sse";
import { DEFAULT_MODEL } from "@/app/lib/modelAliases";
import { PENDING_CONV_KEY } from "./workspaceHref";
import { formatBytes } from "./formatters";
import type { ConversationSummary, MCPServerInfo } from "./chat-experience";
import type { PerConvComposerState } from "./usePerConvComposerState";
import type { TurnStreamState } from "./useTurnStreamState";

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
    options?: { preserveScroll?: boolean },
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
  promoteStreamKey: TurnStreamState["promoteStreamKey"];
  streamingConvsRef: TurnStreamState["streamingConvsRef"];
  isStreaming: boolean;
}

// The public entry points the component/JSX still call. applyStreamEvent,
// pumpStreamResponse, streamTurn, and uploadPendingAttachments are internal
// to the loop and intentionally not returned.
export interface UseTurnStream {
  reattachToConv: (convId: string) => Promise<void>;
  submitPrompt: (submittedPrompt: string) => Promise<void>;
  regenerateLastAssistant: () => Promise<void>;
  resendUserMessage: (userMessageId: number, editedContent: string) => Promise<void>;
  retryLastUserMessage: () => Promise<void>;
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
    promoteStreamKey,
    streamingConvsRef,
    isStreaming,
  } = deps;

  const applyStreamEvent = async (
    event: ServerEvent,
    payload: unknown,
    ctx: { target: string; assistantId: number; thinkingStartedAt: number; hasStartedStreaming: boolean; isReattach: boolean; sawTerminal: boolean },
  ) => {
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
        if (typeof p.model === "string") setSelectedModel(p.model || DEFAULT_MODEL);
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
      const p = payload as { text?: string };
      const text = (p.text ?? "").trim();
      if (!text) return;
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
          { id: p.approval_id, tool: p.tool, summary: p.summary, status: "pending", expiresAt: p.expires_at },
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
      const p = payload as { proposal_id: string; content: string };
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
            { id: p.proposal_id, content: p.content, status: "pending" },
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
        content: m.content || (m.reasoning ? "" : "No response returned."),
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
    ctx: { target: string; assistantId: number; thinkingStartedAt: number; hasStartedStreaming: boolean; isReattach: boolean; sawTerminal: boolean },
  ) => {
    if (!response.body) {
      throw new Error("Empty response body from chat-server.");
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

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

  const reattachToConv = async (convId: string) => {
    if (attachedConvIdsRef.current.has(convId)) return;
    if (reattachInFlightRef.current.has(convId)) return;
    reattachInFlightRef.current.add(convId);
    try {
      const probe = await fetch(`/api/conversations/${convId}/inflight`, { cache: "no-store" });
      if (!probe.ok) return;
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
      if (!info.inflight && !info.turn_id) return;
      if (attachedConvIdsRef.current.has(convId)) return;

      // If the server's still holding a retained buffer for a finished
      // turn but the local cache already shows a completed assistant
      // message at the end, loadConversation has already pulled the
      // canonical history from Postgres — replaying the buffer here
      // would just duplicate every event onto a fresh slot at the end
      // of the conversation. The retain-buffer reattach (PR #94) is
      // for the *missing-events* case: phone locked mid-stream, SSE
      // dropped, browser missed turn.completed, AppendHistory hadn't
      // landed yet. Once the page-reload path fired loadConversation
      // and got the persisted shape, the buffer is redundant. Skip.
      if (!info.inflight && info.turn_id) {
        const existing = messagesByConvRef.current[convId] ?? [];
        const last = existing[existing.length - 1];
        if (last && last.role === "assistant" && last.state === "done") return;
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
      const response = await fetch(`/api/conversations/${convId}/stream${qs}`, {
        method: "GET",
        cache: "no-store",
        headers: { "Last-Event-ID": String(lastSeen) },
      });
      if (!response.ok) return;

      const ctx = {
        target: convId,
        assistantId,
        thinkingStartedAt: nowMs(),
        hasStartedStreaming: false,
        isReattach: true,
        sawTerminal: false,
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
        patchAssistantMessage(convId, ctx.assistantId, (m) =>
          m.state === "thinking" || m.state === "streaming"
            ? { ...m, state: "done", content: m.content || (m.reasoning ? "" : m.content) }
            : m,
        );
        // After reattach ends (turn finished or server hung up), the
        // canonical record is in Postgres; refresh so any server-side
        // state we missed (new title, updated metrics sidebar) shows.
        if (attachedConvIdsRef.current.has(convId)) {
          attachedConvIdsRef.current.delete(convId);
          markConvIdle(convId);
        }
        void refreshConversations();
      }
    } catch {
      // Silent — reattach is best-effort.
      if (attachedConvIdsRef.current.has(convId)) {
        attachedConvIdsRef.current.delete(convId);
        markConvIdle(convId);
      }
    } finally {
      reattachInFlightRef.current.delete(convId);
    }
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
      content: m.content || (m.reasoning ? "" : "No response returned."),
      state: "done",
    }));
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
    // Per-conv streaming gate: only block when the conv the user is
    // about to submit into is itself busy. Other in-flight chats
    // (sidebar dots) keep running undisturbed.
    if (!value || (convId && streamingConvsRef.current.has(convId)) || !userEmail) return;
    if (modelError) return;

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
      const enabledOptional = mcpServers.filter((s) => s.enabled).map((s) => s.name);
      if (enabledOptional.length > 0) {
        body.enabled_optional = enabledOptional;
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
          // Defensive reconcile against the probe/reattach race: if
          // the turn completed between our /inflight probe and
          // reattach's own probe, reattach short-circuits without
          // attaching and the slot is left marked "streaming". Reload
          // from the DB to surface the canonical final state.
          const slot = messagesByConvRef.current[target]?.find((m) => m.id === assistantId);
          if (slot && (slot.state === "streaming" || slot.state === "thinking")) {
            // Either mid-flight state means reattach short-circuited
            // (e.g. the retain buffer was already evicted) without
            // delivering a terminal event. Postgres has the canonical
            // shape — pull it.
            await loadConversation(target);
          }
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
            // already been persisted to Postgres and its short retain buffer
            // (server.go:bufferRetainTTL, ~2m) has since expired — so
            // /inflight legitimately reports nothing even though the full
            // answer exists in the DB. That's the "looks failed until I
            // refresh" report: a manual refresh recovers it because it reads
            // Postgres. Do the same here BEFORE declaring failure — only
            // stamp "failed" when the DB confirms there's no completed answer.
            let recovered = false;
            if (!isPendingKey(target)) {
              try {
                const r = await fetch(`/api/conversations/${target}`, { cache: "no-store" });
                if (r.ok) {
                  const data = (await r.json()) as { history?: HistoryEntry[] | null };
                  const persisted = historyToMessages(data.history ?? []);
                  const last = persisted[persisted.length - 1];
                  recovered = Boolean(
                    last && last.role === "assistant" && last.state === "done" && !last.failed,
                  );
                }
              } catch {
                /* DB probe failed — fall through to the failed marker */
              }
            }
            if (recovered) {
              // Release our attach handle so loadConversation doesn't
              // short-circuit, then reload the canonical state from Postgres
              // — identical to a manual page refresh, but automatic.
              attachedConvIdsRef.current.delete(target);
              await loadConversation(target);
            } else {
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
      attachedConvIdsRef.current.delete(finalTarget);
      markConvIdle(finalTarget);
      // Belt-and-suspenders: if any path missed transitioning the slot
      // out of a mid-flight state, do it here. The success path
      // already patches `state: "done"` after pumpStreamResponse
      // returns; the failure path patches `failed: true` or
      // hands off to reattach. This catches the gap where neither
      // fired — historically observed as the indicator hanging until
      // the user refreshed the page. Stamping done is safe: any
      // already-terminal state (done/failed/cancelled) is left alone.
      patchAssistantMessage(finalTarget, assistantId, (m) =>
        m.state === "thinking" || m.state === "streaming"
          ? { ...m, state: "done" }
          : m,
      );
    }
  };

  return {
    reattachToConv,
    submitPrompt,
    regenerateLastAssistant,
    resendUserMessage,
    retryLastUserMessage,
  };
}
