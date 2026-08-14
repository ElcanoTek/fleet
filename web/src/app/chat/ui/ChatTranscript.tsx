"use client";

// ChatTranscript — the scrollable conversation transcript extracted from
// ChatExperience (issue #169 decomposition, slice 8). This is a *presentational*
// component: it owns no turn/send/SSE state machine of its own. Every value,
// setter, ref, and callback it needs is threaded in as a prop from
// ChatExperience, which keeps owning the per-conversation message state, the
// streaming turn loop, and all data loading. The JSX below is the former inline
// `<section ref={conversationRef}>` block, moved verbatim so the live specs
// (message rendering, thinking indicator, approval/memory cards, edit/resend,
// retry/regenerate, compaction expander) keep driving identical DOM.
//
// The three message-render helpers below — UserBubble, ReasoningBlock, and
// SummarizeProgressCard — were module-level functions in chat-experience whose
// only callers are this transcript JSX, so they move here verbatim (the
// originals are removed from chat-experience to keep one definition each).
import { lazy, Suspense, useEffect, useMemo, useRef, useState } from "react";
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { LoadingLogo } from "./LoadingLogo";
import { EmptyStatePrompts, ProtocolPillForm } from "./EmptyStatePrompts";
import { getPill, type ProtocolPill } from "./protocolPills";
import { Icon } from "./Icon";
import { PythonOutput, ToolChip, taskTrackerDisplayForMessage } from "./ToolChips";
import { CopyButton, SummaryBanner, TurnSummaryChip } from "./ChatChips";
import { ApprovalCard, MemoryProposalCard } from "./ApprovalCards";
import { MessageMinimap, MINIMAP_MAX, type MinimapEntry } from "./MessageMinimap";
// The assistant markdown pipeline (react-markdown + micromark, ~43 KiB
// transfer) is lazy-loaded: nothing renders it until a transcript message is
// on screen, so the first-load critical path (hydrate + boot fetch + paint
// the empty state or transcript shell) doesn't pay its download/parse cost.
// While the chunk loads, messages show their raw text with preserved
// whitespace — same box, same type metrics — then upgrade in place.
const AssistantMarkdown = lazy(() => import("./AssistantContent"));

function MessageMarkdown({
  content,
  isStreaming,
  conversationId,
}: {
  content: string;
  isStreaming?: boolean;
  conversationId?: string | null;
}) {
  if (!content.trim()) return null;
  return (
    <Suspense fallback={<div className="whitespace-pre-wrap">{content}</div>}>
      <AssistantMarkdown
        content={content}
        isStreaming={isStreaming}
        conversationId={conversationId}
      />
    </Suspense>
  );
}
import { humanToolLabel, shortModelName, type Message } from "./history";

export type ChatTranscriptProps = {
  // Scroll container + bottom sentinel refs (owned by ChatExperience)
  conversationRef: RefObject<HTMLElement | null>;
  streamEndRef: RefObject<HTMLDivElement | null>;
  promptRef: RefObject<HTMLTextAreaElement | null>;

  // Load / empty / lockdown gating
  isLoadingHistory: boolean;
  isLockdown: boolean;
  messages: Message[];

  // Empty-state protocol pills
  pills: ProtocolPill[];
  activePillId: string | null;
  setActivePillId: Dispatch<SetStateAction<string | null>>;
  submitPrompt: (submittedPrompt: string) => void | Promise<void>;
  setPrompt: Dispatch<SetStateAction<string>>;

  // Compaction / summarize
  isSummarizing: boolean;
  summarizeStartedAt: number | null;
  summarizeStream: string;
  summarizeError: string | null;
  summaryIndex: number;
  summaryExpanded: boolean;
  setSummaryExpanded: Dispatch<SetStateAction<boolean>>;
  setConfirmSummarize: Dispatch<SetStateAction<boolean>>;

  // Per-message render context
  showStats: boolean;
  crossfadingMessageIds: number[];
  currentConvKey: string;
  realConvId: (key: string | null) => string | null;
  isStreaming: boolean;
  lastUserMessageId: number | null;
  lastAssistantMessageId: number | null;
  // Keyboard `e` shortcut (#306): a bumped nonce paired with the target message
  // id, asking that message to enter its inline editor. UserBubble owns the edit
  // state, so the request arrives as a signal it watches. Pairing with the id
  // (not a bare counter) means only the explicitly-requested message opens.
  // null = no request yet.
  editLastUserSignal: { id: number; nonce: number } | null;
  selectedModel: string;

  // Turn / message actions
  patchAssistantMessage: (
    convId: string,
    assistantId: number,
    updater: (message: Message) => Message,
  ) => void;
  resendUserMessage: (userMessageId: number, editedContent: string) => void | Promise<void>;
  retryLastUserMessage: () => void | Promise<void>;
  regenerateLastAssistant: () => void | Promise<void>;
  // Fork the conversation at a persisted message into a new thread (#454).
  branchFromMessage: (message: Message) => void | Promise<void>;
  loadMemories: () => void | Promise<void>;

  // Model picker / model-required affordance
  setSelectedModel: Dispatch<SetStateAction<string>>;
  setModelPickerOpen: Dispatch<SetStateAction<boolean>>;
  setModelSearchQuery: Dispatch<SetStateAction<string>>;
  loadRankedModels: () => void | Promise<void>;
  loadCatalogModels: () => void | Promise<void>;
};

// The transcript is virtualized (@tanstack/react-virtual): only the rows near
// the viewport are mounted, so a long conversation no longer pays to render
// react-markdown + react-syntax-highlighter for every message on every keystroke
// or scroll. To feed a flat, stable-indexed list to the virtualizer, the former
// inline `messages.map` — with its compaction-boundary early-returns and the
// leading summarize card/error — is flattened into this explicit row model.
type TranscriptRow =
  // Live compaction progress card (only while summarizing).
  | { kind: "summarizing" }
  // Compaction error banner.
  | { kind: "summarize-error" }
  // The single "Show N earlier turns" expander that stands in for the whole
  // collapsed pre-summary range.
  | { kind: "expander" }
  // A compaction summary banner message.
  | { kind: "summary"; message: Message }
  // A normal user/assistant turn. `isPreSummary` dims pre-compaction turns when
  // the range is expanded (matching the former inline opacity-60 rule).
  | { kind: "message"; message: Message; isPreSummary: boolean };

// VirtualTranscriptRow is the absolutely-positioned wrapper the virtualizer
// places each visible row into. It also owns the entrance-animation gate:
// because virtualization mounts/unmounts rows as they scroll past the viewport,
// applying the `message-enter-*` class unconditionally would replay the 200ms
// slide every time a row re-entered view. Instead a row plays its entrance only
// the first time its message id is ever shown (tracked in the shared `seenIds`
// set, marked after render by ChatTranscript). The decision is captured in lazy
// state so it stays stable across the streaming re-renders of a live turn —
// mirroring the pre-virtualization behavior where the class lived on one
// persistent DOM node and fired once on mount.
function VirtualTranscriptRow({
  index,
  start,
  measureElement,
  animationId,
  animationClass,
  seenIds,
  children,
}: {
  index: number;
  start: number;
  measureElement: (node: HTMLDivElement | null) => void;
  animationId: number | undefined;
  animationClass: string | undefined;
  seenIds: Set<number>;
  children: (enterClass: string) => ReactNode;
}) {
  const [animate] = useState(
    () => animationId !== undefined && !seenIds.has(animationId),
  );
  const enterClass = animate ? animationClass ?? "" : "";
  return (
    <div
      data-index={index}
      ref={measureElement}
      className="min-w-0"
      style={{
        position: "absolute",
        top: 0,
        left: 0,
        width: "100%",
        transform: `translateY(${start}px)`,
      }}
    >
      {children(enterClass)}
    </div>
  );
}

export function ChatTranscript({
  conversationRef,
  streamEndRef,
  promptRef,
  isLoadingHistory,
  isLockdown,
  messages,
  pills,
  activePillId,
  setActivePillId,
  submitPrompt,
  setPrompt,
  isSummarizing,
  summarizeStartedAt,
  summarizeStream,
  summarizeError,
  summaryIndex,
  summaryExpanded,
  setSummaryExpanded,
  setConfirmSummarize,
  showStats,
  crossfadingMessageIds,
  currentConvKey,
  realConvId,
  isStreaming,
  lastUserMessageId,
  lastAssistantMessageId,
  editLastUserSignal,
  selectedModel,
  patchAssistantMessage,
  resendUserMessage,
  retryLastUserMessage,
  regenerateLastAssistant,
  branchFromMessage,
  loadMemories,
  setSelectedModel,
  setModelPickerOpen,
  setModelSearchQuery,
  loadRankedModels,
  loadCatalogModels,
}: ChatTranscriptProps) {
  "use no memo";
  // TanStack Virtual returns imperative functions whose identities cannot be
  // safely memoized by React Compiler. Keep this component opted out until the
  // library exposes a compiler-compatible API; its own virtualizer still
  // provides the transcript's intended rendering optimization.
  // Flatten the message list (plus the leading summarize card/error and the
  // compaction expander) into the explicit, stable-indexed row model the
  // virtualizer consumes. This reproduces the former inline map's control flow:
  // when a compaction summary exists and the pre-summary range is collapsed,
  // the whole range [0, summaryIndex) is represented by a single expander row.
  const rows = useMemo<TranscriptRow[]>(() => {
    const out: TranscriptRow[] = [];
    if (isSummarizing) out.push({ kind: "summarizing" });
    if (summarizeError) out.push({ kind: "summarize-error" });
    const collapsed = summaryIndex >= 0 && !summaryExpanded;
    messages.forEach((message, idx) => {
      const isPreSummary = summaryIndex >= 0 && idx < summaryIndex;
      if (isPreSummary && collapsed) {
        // The single expander stands in for the first hidden turn; every other
        // hidden turn is skipped (matches the former `return null`).
        if (idx === 0) out.push({ kind: "expander" });
        return;
      }
      if (message.kind === "summary") {
        out.push({ kind: "summary", message });
        return;
      }
      out.push({ kind: "message", message, isPreSummary });
    });
    return out;
  }, [messages, summaryIndex, summaryExpanded, isSummarizing, summarizeError]);

  // Message ids whose entrance animation has already been accounted for. Marked
  // after every render (see below), so a row re-mounting on scroll never
  // replays its slide-in — only genuinely new turns animate. See
  // VirtualTranscriptRow for how the gate is applied.
  const animatedIdsRef = useRef<Set<number>>(new Set());
  useEffect(() => {
    for (const message of messages) animatedIdsRef.current.add(message.id);
  }, [messages]);

  // Inter-row gap, matched to the former `gap-5 sm:gap-6` grid (20px / 24px at
  // the 640px `sm` breakpoint). Fed to the virtualizer so measured offsets and
  // the total size include the spacing. Lazy-initialized from the current
  // breakpoint and only updated on media-query changes, so it never sets state
  // during the effect body.
  const [rowGap, setRowGap] = useState(() =>
    typeof window !== "undefined" &&
    window.matchMedia("(min-width: 640px)").matches
      ? 24
      : 20,
  );
  useEffect(() => {
    const mq = window.matchMedia("(min-width: 640px)");
    const onChange = (event: MediaQueryListEvent) => setRowGap(event.matches ? 24 : 20);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  // The virtualizer scrolls inside the existing conversation <section> (the same
  // element the parent's auto-scroll / snap-to-bottom effects drive), so those
  // behaviors keep working unchanged: the spacer's height feeds scrollHeight and
  // the streamEndRef sentinel still sits at the very bottom. Rows are
  // dynamically measured (variable-height markdown/code) via measureElement;
  // unmeasured rows use a running average around this estimate.
  // eslint-disable-next-line react-hooks/incompatible-library -- TanStack Virtual is intentionally isolated in this no-memo component.
  const virtualizer = useVirtualizer<HTMLElement, HTMLDivElement>({
    count: rows.length,
    getScrollElement: () => conversationRef.current,
    estimateSize: () => 180,
    overscan: 6,
    gap: rowGap,
    getItemKey: (index) => {
      const row = rows[index];
      switch (row.kind) {
        case "summarizing":
          return "row:summarizing";
        case "summarize-error":
          return "row:summarize-error";
        case "expander":
          return "row:expander";
        case "summary":
          return `row:summary:${row.message.id}`;
        case "message":
          return `row:message:${row.message.id}`;
      }
    },
  });

  // ── Message minimap (Codex-style jump rail) ────────────────────────────────
  // One dash per user message currently present in the row model (a collapsed
  // pre-summary turn has no row, so it can't be a jump target), capped to the
  // most recent MINIMAP_MAX. Each entry carries its row index for
  // scrollToIndex and the start of the assistant's reply for the hover card.
  const minimapEntries = useMemo<
    Array<MinimapEntry & { rowIndex: number }>
  >(() => {
    // Pair each user message with the first assistant reply that follows it.
    const replyByUserId = new Map<number, string>();
    let pendingUserId: number | null = null;
    for (const m of messages) {
      if (m.kind === "summary") continue;
      if (m.role === "user") pendingUserId = m.id;
      else if (m.role === "assistant" && pendingUserId != null && m.content) {
        replyByUserId.set(pendingUserId, m.content.slice(0, 280));
        pendingUserId = null;
      }
    }
    const out: Array<MinimapEntry & { rowIndex: number }> = [];
    rows.forEach((row, rowIndex) => {
      if (row.kind !== "message" || row.message.role !== "user") return;
      out.push({
        id: row.message.id,
        rowIndex,
        userText: row.message.content,
        replySnippet: replyByUserId.get(row.message.id),
      });
    });
    return out.slice(-MINIMAP_MAX);
  }, [rows, messages]);

  // The dash under the reading position: the last user turn whose row starts
  // above ~1/3 of the viewport. Tracked from the scroller (rAF-throttled);
  // getOffsetForIndex works for unmounted rows, so this needs no querySelector.
  const [minimapActiveId, setMinimapActiveId] = useState<number | null>(null);
  useEffect(() => {
    const el = conversationRef.current;
    if (!el || minimapEntries.length === 0) return;
    let raf = 0;
    const update = () => {
      raf = 0;
      const probe = el.scrollTop + el.clientHeight * 0.33;
      let current: number | null = minimapEntries[0]?.id ?? null;
      for (const entry of minimapEntries) {
        const offset = virtualizer.getOffsetForIndex(entry.rowIndex, "start")?.[0];
        if (offset != null && offset <= probe) current = entry.id;
      }
      setMinimapActiveId((prev) => (prev === current ? prev : current));
    };
    const onScroll = () => {
      if (!raf) raf = requestAnimationFrame(update);
    };
    onScroll();
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      el.removeEventListener("scroll", onScroll);
      if (raf) cancelAnimationFrame(raf);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [minimapEntries, conversationRef]);

  const jumpToMessage = (id: number) => {
    const entry = minimapEntries.find((e) => e.id === id);
    if (!entry) return;
    // Instant (not smooth): dynamic row measurement makes smooth scrolling
    // land short, and the scrub gesture needs immediate response anyway.
    virtualizer.scrollToIndex(entry.rowIndex, { align: "start" });
    setMinimapActiveId(id);
  };

  return (
          <section
            ref={conversationRef}
            aria-label="Conversation"
            role="region"
            // overflow-x-hidden: belt-and-suspenders against a stray wide
            // child (long unbreakable token, syntax-highlighted long line)
            // creating a horizontal scroll inside the chat column on mobile.
            // The min-w-0 chain below should already prevent it, but if a
            // future renderer regresses, this caps the visible bleed.
            className="min-h-0 min-w-0 overflow-x-hidden overflow-y-auto pr-0 sm:pr-1"
          >
            {/* Jump rail: sticky (height 0, so the column below is unshifted)
                pinned to the scroller's visible top; the rail hangs in the
                left gutter from ~18vh down. Desktop-only — hover previews and
                press-drag scrubbing have no touch equivalent. */}
            {minimapEntries.length >= 2 ? (
              <div className="pointer-events-none sticky top-0 z-10 hidden h-0 sm:block">
                <div className="pointer-events-auto absolute left-0 top-[18vh]">
                  <MessageMinimap
                    entries={minimapEntries}
                    activeId={minimapActiveId}
                    onJump={jumpToMessage}
                  />
                </div>
              </div>
            ) : null}
            <div className="mx-auto grid min-h-full w-full min-w-0 max-w-[53rem] content-start gap-3 pb-4 sm:gap-4 sm:pb-6">
              {isLoadingHistory ? (
                <div className="flex min-h-full items-center justify-center pb-8 text-[0.875rem] text-[var(--color-text-muted)]">
                  Loading conversation...
                </div>
              ) : messages.length === 0 ? (
                <div className="flex min-h-full flex-col items-center justify-start gap-6 pb-6 pt-10 sm:gap-8 sm:pb-8">
                  <div className="text-center">
                    <h2 className="font-heading text-[1.4rem] font-semibold text-[var(--color-text-primary)] sm:text-[1.75rem]">
                      {isLockdown ? "Lockdown chat" : "What can I help with?"}
                    </h2>
                    {isLockdown ? (
                      <p className="mt-2 max-w-md text-[0.8125rem] text-[var(--color-text-muted)] sm:text-[0.875rem]">
                        Sealed and private. Your data and an approved model
                        stay inside this sandbox — nothing leaves. Open a
                        standard chat when you need full web access.
                      </p>
                    ) : null}
                  </div>
                  {!isLockdown && pills.length > 0 ? (
                    activePillId && getPill(activePillId, pills) ? (
                      <ProtocolPillForm
                        pill={getPill(activePillId, pills)!}
                        onRun={(promptText) => {
                          setActivePillId(null);
                          void submitPrompt(promptText);
                        }}
                        onStartChat={(starter) => {
                          setActivePillId(null);
                          void submitPrompt(starter);
                        }}
                        onDescribe={(preload) => {
                          setActivePillId(null);
                          setPrompt(preload);
                          requestAnimationFrame(() => promptRef.current?.focus());
                        }}
                        onCancel={() => setActivePillId(null)}
                      />
                    ) : (
                      <EmptyStatePrompts
                        pills={pills}
                        onPick={(id) => {
                          const picked = getPill(id, pills);
                          if (!picked) return;
                          // A pure conversation pill jumps straight into the
                          // intake; form pills (and the diagnostic's optional
                          // form) open the inline panel first.
                          if (picked.type === "conversation" && !picked.optionalForm) {
                            void submitPrompt(picked.starterPrompt ?? "");
                          } else {
                            setActivePillId(id);
                          }
                        }}
                      />
                    )
                  ) : null}
                </div>
              ) : (
                <div className="min-w-0">
                  {/* Virtualized transcript: only rows near the viewport are
                      mounted. The spacer div carries the full measured height so
                      the parent's scrollHeight-based auto-scroll + snap-to-bottom
                      effects (driven off conversationRef) and the streamEndRef
                      sentinel below keep behaving exactly as before. */}
                  <div
                    className="relative min-w-0"
                    style={{ height: virtualizer.getTotalSize(), width: "100%" }}
                  >
                    {virtualizer.getVirtualItems().map((virtualRow) => {
                      const row = rows[virtualRow.index];
                      const animationId =
                        row.kind === "message" ? row.message.id : undefined;
                      const animationClass =
                        row.kind === "message"
                          ? row.message.role === "user"
                            ? "message-enter-user"
                            : "message-enter-assistant"
                          : undefined;
                      return (
                        <VirtualTranscriptRow
                          key={virtualRow.key}
                          index={virtualRow.index}
                          start={virtualRow.start}
                          measureElement={virtualizer.measureElement}
                          animationId={animationId}
                          animationClass={animationClass}
                          seenIds={animatedIdsRef.current}
                        >
                          {(enterClass) => {
                            if (row.kind === "summarizing") {
                              return (
                                <SummarizeProgressCard
                                  startedAt={summarizeStartedAt}
                                  streamingText={summarizeStream}
                                />
                              );
                            }
                            if (row.kind === "summarize-error") {
                              return (
                                <div
                                  role="alert"
                                  className="rounded-[0.6rem] border border-[var(--color-danger)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-danger)]"
                                >
                                  {summarizeError}
                                </div>
                              );
                            }
                            if (row.kind === "expander") {
                              return (
                                <button
                                  type="button"
                                  className="mx-auto flex w-full items-center justify-center gap-2 rounded-[0.6rem] border border-dashed border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.75rem] text-[var(--color-text-muted)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)]"
                                  aria-expanded={false}
                                  onClick={() => setSummaryExpanded(true)}
                                >
                                  <Icon name="chevron-down" className="size-3" />
                                  Show {summaryIndex} earlier turn{summaryIndex === 1 ? "" : "s"} — compacted below
                                </button>
                              );
                            }
                            if (row.kind === "summary") {
                              return (
                                <SummaryBanner
                                  message={row.message}
                                  collapsedRangeCount={summaryIndex}
                                  summaryExpanded={summaryExpanded}
                                  onToggleExpand={() => setSummaryExpanded((v) => !v)}
                                  onResummarize={() => setConfirmSummarize(true)}
                                  isSummarizing={isSummarizing}
                                />
                              );
                            }
                            const message = row.message;
                            const isPreSummary = row.isPreSummary;
                            const taskTrackerDisplay = taskTrackerDisplayForMessage(message);
                    const activeTaskTitle = taskTrackerDisplay?.activeTask ?? null;
                    const toolCalls = message.toolCalls ?? [];
                    const pythonStreams = message.pythonStreams ?? [];
                    const hasExecutionTrail = toolCalls.length > 0 || pythonStreams.length > 0;
                    // With "Show details" on, the tool-call pills
                    // stream in live so someone debugging a stuck agent
                    // can see which tool is in flight. With it off, the
                    // "Thinking…" indicator (named with the active tool
                    // when we have one) is the only signal. Either way,
                    // the stats chip waits until the turn completes
                    // since its numbers don't exist until then.
                    const showExecutionTrail = showStats && hasExecutionTrail;
                    // Two signals feed the thinking indicator:
                    //
                    //   activeToolName — the most recent tool call's
                    //     name. Prefer one that's still pending (it's
                    //     literally what the agent is waiting on right
                    //     now), but fall back to the latest call
                    //     regardless of state so the brief gap between
                    //     `tool.result done` and the next `tool.call`
                    //     doesn't blank the indicator to "Thinking".
                    //   activeTaskTitle — the task_tracker's
                    //     in-progress task title (the agent's stated
                    //     intention for the current step). Updates
                    //     only when the agent re-calls task_tracker,
                    //     so it can lag the live tool by several
                    //     seconds.
                    //
                    // The render below shows tool name as the primary
                    // live signal (with dots) and task title as
                    // secondary context — picking task title alone
                    // (the previous behavior) made the indicator
                    // appear to "stick" on whatever step the agent
                    // last marked in_progress, e.g. "Authenticate to
                    // Index Exchange" while the agent was actually
                    // calling list_reports / pull_report.
                    let activeToolName: string | null = null;
                    for (let j = toolCalls.length - 1; j >= 0; j--) {
                      if (toolCalls[j].state === "pending") {
                        activeToolName = toolCalls[j].name;
                        break;
                      }
                    }
                    if (!activeToolName && toolCalls.length > 0) {
                      activeToolName = toolCalls[toolCalls.length - 1].name;
                    }
                    return (
                      <article
                        className={[
                          // min-w-0 lets this grid item shrink to its track
                          // instead of expanding to fit a wide descendant
                          // (long URL in a user bubble, long stdout line in
                          // a tool result). Without it the item's default
                          // min-width:auto pushes the whole chat column
                          // past the viewport on mobile.
                          "flex min-w-0 gap-3",
                          message.role === "user" ? "justify-end" : "justify-start",
                          // Entrance animation — gated by VirtualTranscriptRow so
                          // it plays once per turn and never replays when a row
                          // re-mounts on scroll. Empty string when not animating.
                          enterClass,
                          // Pre-summary turns visible only when expanded;
                          // dim them so users grok they are reference
                          // history and not part of the model's live
                          // context anymore.
                          isPreSummary ? "opacity-60" : "",
                        ].join(" ")}
                      >
                        <div className={message.role === "user" ? "min-w-0 max-w-[88%] sm:max-w-[72%]" : "min-w-0 flex-1"}>
                          {message.role === "user" ? (
                            <UserBubble
                              message={message}
                              isLastUser={message.id === lastUserMessageId}
                              isStreaming={isStreaming}
                              editRequestSignal={
                                editLastUserSignal && editLastUserSignal.id === message.id
                                  ? editLastUserSignal.nonce
                                  : 0
                              }
                              onResend={(edited) => void resendUserMessage(message.id, edited)}
                            />
                          ) : (
                            <div className="relative min-h-[1.75rem] min-w-0">
                            <div
                              className="grid min-w-0 gap-2 text-[0.9rem] leading-[1.55] text-[var(--color-text-primary)] sm:gap-2.5 sm:text-[0.96875rem] sm:leading-[1.6]"
                            >
                              {/*
                                Render order is chronological-then-actionable:
                                  1. Execution trail (tool chips + python streams) — what the agent did.
                                  2. Reasoning block — the agent's narrated thinking, if "show details" is on.
                                  3. Thinking indicator — the live "currently working" cue (only fires before any text).
                                  4. Text + images — the actual response.
                                  5. Status banners (retrying / cancelled / failed / modelRequired) — what happened to this turn.
                                  6. Turn summary chip — token/cost metrics, if "show details" is on.
                                  7. Approval + memory cards — the call-to-action for the user.
                                  8. Copy / regenerate footer — utility actions on the finished response.
                                Anything that needs the human's attention sits at the bottom so the
                                eye lands on it last. Anything that traces past work sits at the top
                                so it doesn't compete with the answer for the reader's first glance.
                              */}
                              {showExecutionTrail ? (
                                <div className="grid gap-1.5 pb-0.5">
                                  {toolCalls.length > 0 ? (
                                    <div className="flex flex-wrap gap-1.5">
                                      {toolCalls.map((tc) => (
                                        <ToolChip
                                          key={tc.id}
                                          tc={tc}
                                          taskTrackerDisplay={taskTrackerDisplay}
                                          conversationId={realConvId(currentConvKey)}
                                        />
                                      ))}
                                    </div>
                                  ) : null}
                                  {pythonStreams.length > 0 ? (
                                    <div className="grid gap-1.5">
                                      {pythonStreams.map((stream, i) => (
                                        <PythonOutput
                                          key={i}
                                          stream={stream}
                                          conversationId={realConvId(currentConvKey) ?? ""}
                                        />
                                      ))}
                                    </div>
                                  ) : null}
                                </div>
                              ) : null}

                              {showStats && message.reasoning ? (
                                <ReasoningBlock text={message.reasoning} />
                              ) : null}

                              {(message.state === "thinking" || crossfadingMessageIds.includes(message.id)) && !message.content ? (
                                <div
                                  className={[
                                    "flex min-w-0 items-center gap-2 text-[0.875rem] text-[var(--color-text-muted)] transition-opacity duration-base",
                                    message.state === "thinking"
                                      ? "opacity-100"
                                      : "pointer-events-none opacity-0",
                                  ].join(" ")}
                                >
                                  {/*
                                    The animated logo orbit IS the "we're working" cue — the
                                    earlier `thinking-dots` ellipsis was redundant next to it,
                                    so it's gone. Truncation priority is reversed: the task
                                    description (the persistent context the user picked) stays
                                    intact at content width via `shrink-0 whitespace-nowrap`,
                                    and the tool label (a short generic verb-phrase like
                                    "Reading file") gets `min-w-0 flex-1 truncate` so it
                                    shrinks/ellipsizes first when the row is tight.
                                  */}
                                  <LoadingLogo size={20} />
                                  <span className="flex min-w-0 items-center gap-1.5">
                                    {activeTaskTitle ? (
                                      <span className="thinking-shimmer shrink-0 whitespace-nowrap" title={activeTaskTitle}>
                                        {activeTaskTitle}
                                      </span>
                                    ) : null}
                                    {activeTaskTitle && activeToolName ? (
                                      <span className="shrink-0 opacity-50" aria-hidden="true">
                                        ·
                                      </span>
                                    ) : null}
                                    {activeToolName ? (
                                      <span className="thinking-shimmer min-w-0 flex-1 truncate">
                                        {humanToolLabel(activeToolName)}
                                      </span>
                                    ) : null}
                                    {!activeTaskTitle && !activeToolName ? (
                                      <span className="thinking-shimmer shrink-0">Thinking</span>
                                    ) : null}
                                  </span>
                                </div>
                              ) : null}

                              <MessageMarkdown
                                content={message.content}
                                isStreaming={
                                  message.state === "streaming" || message.state === "thinking"
                                }
                                conversationId={realConvId(currentConvKey)}
                              />
                              {(message.state === "streaming" ||
                                (message.state === "thinking" && message.reasoning)) &&
                              message.content ? (
                                <LoadingLogo size={18} className="mt-1 opacity-60" />
                              ) : null}

                              {message.retrying ? (
                                <div
                                  className="flex items-center gap-2 text-[0.75rem] text-[var(--color-text-muted)]"
                                  title={message.retrying.message || undefined}
                                >
                                  <span className="inline-block size-1.5 animate-pulse rounded-full bg-[var(--color-text-muted)]" />
                                  Retrying
                                  {message.retrying.statusCode
                                    ? ` (HTTP ${message.retrying.statusCode})…`
                                    : "…"}
                                  {message.retrying.delayMs > 0 ? (
                                    <span className="text-[var(--color-text-muted)]">
                                      next attempt in {Math.max(1, Math.round(message.retrying.delayMs / 1000))}s
                                    </span>
                                  ) : null}
                                </div>
                              ) : null}

                              {message.contextPressure ? (
                                <div
                                  className="flex items-center gap-2 rounded-[0.75rem] border border-[var(--color-warning-border)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.75rem] text-[var(--color-text-secondary)]"
                                  title={`${message.contextPressure.usedTokens.toLocaleString()} of ${message.contextPressure.windowSize.toLocaleString()} tokens`}
                                >
                                  <span className="inline-block size-1.5 shrink-0 rounded-full bg-[var(--color-warning)]" />
                                  Conversation is {Math.round(message.contextPressure.pct * 100)}% full — consider
                                  starting a new session for complex tasks.
                                </div>
                              ) : null}

                              {message.contextCompacted ? (
                                <div className="flex items-center gap-2 rounded-[0.75rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.75rem] text-[var(--color-text-secondary)]">
                                  <span className="inline-block size-1.5 shrink-0 rounded-full bg-[var(--color-accent)]" />
                                  Older context was summarized to make room
                                  {message.contextCompacted.removedTurns > 0
                                    ? ` (${message.contextCompacted.removedTurns} earlier ${
                                        message.contextCompacted.removedTurns === 1 ? "message" : "messages"
                                      }).`
                                    : "."}
                                </div>
                              ) : null}

                              {message.cancelled ? (
                                <div className="flex items-center gap-2 text-[0.75rem] text-[var(--color-text-muted)]">
                                  <span className="inline-block size-1.5 rounded-full bg-[var(--color-text-muted)]" />
                                  Turn stopped. <button
                                    type="button"
                                    className="underline hover:text-[var(--color-text-primary)]"
                                    onClick={() => void retryLastUserMessage()}
                                  >
                                    Retry
                                  </button>
                                </div>
                              ) : null}

                              {message.modelRequired ? (() => {
                                // context_too_large is the only reason where retrying with the
                                // same model definitely won't help — the conversation simply
                                // doesn't fit. fatal and retry_exhausted are usually transient
                                // (e.g. an OpenRouter 400 mid-stream that resolves on the next
                                // attempt), so retry is the primary action and the model
                                // picker is a fallback.
                                const reason = message.modelRequired.reason;
                                const mustSwap = reason === "context_too_large";
                                return (
                                <div className="flex flex-col gap-1.5 rounded-[0.75rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] p-3 text-[0.75rem] text-[var(--color-text-secondary)]">
                                  <div className="flex items-center gap-2">
                                    <span className="inline-block size-1.5 rounded-full bg-[var(--color-danger)]" />
                                    <span className="font-medium text-[var(--color-text-primary)]">
                                      {mustSwap ? "Pick a different model" : "Turn failed"}
                                    </span>
                                    {message.modelRequired.failedModel ? (
                                      <span className="truncate text-[var(--color-text-muted)]">
                                        {shortModelName(message.modelRequired.failedModel)}
                                      </span>
                                    ) : null}
                                  </div>
                                  <p>{message.modelRequired.message}</p>
                                  <div className="flex flex-wrap items-center gap-3 pt-0.5">
                                    {mustSwap ? (
                                      <>
                                        <button
                                          type="button"
                                          className="underline hover:text-[var(--color-text-primary)]"
                                          onClick={() => {
                                            setModelPickerOpen(true);
                                            setModelSearchQuery("");
                                            void loadRankedModels();
                                            void loadCatalogModels();
                                          }}
                                        >
                                          Open model picker
                                        </button>
                                        <button
                                          type="button"
                                          className="underline hover:text-[var(--color-text-primary)]"
                                          onClick={() => void retryLastUserMessage()}
                                        >
                                          Retry with {shortModelName(selectedModel)}
                                        </button>
                                      </>
                                    ) : (
                                      <>
                                        <button
                                          type="button"
                                          className="font-medium text-[var(--color-text-primary)] underline hover:text-[var(--color-accent)]"
                                          onClick={() => void retryLastUserMessage()}
                                        >
                                          Retry with {shortModelName(selectedModel)}
                                        </button>
                                        <button
                                          type="button"
                                          className="underline hover:text-[var(--color-text-primary)]"
                                          onClick={() => {
                                            setModelPickerOpen(true);
                                            setModelSearchQuery("");
                                            void loadRankedModels();
                                            void loadCatalogModels();
                                          }}
                                        >
                                          or pick a different model
                                        </button>
                                      </>
                                    )}
                                  </div>
                                </div>
                                );
                              })() : message.failed ? (
                                <div className="flex items-center gap-2 text-[0.75rem] text-[var(--color-danger)]">
                                  <span className="inline-block size-1.5 rounded-full bg-[var(--color-danger)]" />
                                  Turn failed. <button
                                    type="button"
                                    className="underline hover:text-[var(--color-text-primary)]"
                                    onClick={() => void retryLastUserMessage()}
                                  >
                                    Retry
                                  </button>
                                </div>
                              ) : null}

                              {/*
                                Empty-reply safety net. A turn can complete
                                with no written answer — e.g. Gemini Flash
                                stops after a run of tool calls without
                                summarizing (reported as "it keeps stopping" /
                                "i am not getting any responses"). The server
                                now forces a final summary in that case, but
                                this catches anything that still slips through
                                (incl. older persisted turns) so the user never
                                sees a blank assistant bubble. Suppressed when
                                another affordance already explains the state
                                (cancelled/failed/model-required/retrying) or
                                owns the turn (approval / memory cards).
                              */}
                              {message.state === "done" &&
                              !message.content.trim() &&
                              !message.cancelled &&
                              !message.failed &&
                              !message.modelRequired &&
                              !message.retrying &&
                              !(message.approvals && message.approvals.length) &&
                              !(message.memoryProposals && message.memoryProposals.length) ? (
                                <div className="flex items-center gap-2 text-[0.75rem] text-[var(--color-text-muted)]">
                                  <span className="inline-block size-1.5 rounded-full bg-[var(--color-text-muted)]" />
                                  The assistant finished without a written reply.{" "}
                                  <button
                                    type="button"
                                    className="underline hover:text-[var(--color-text-primary)]"
                                    onClick={() => void retryLastUserMessage()}
                                  >
                                    Retry
                                  </button>
                                </div>
                              ) : null}

                              {showStats && message.summary ? (
                                <TurnSummaryChip
                                  summary={message.summary}
                                  toolCalls={toolCalls}
                                  pythonStreams={pythonStreams}
                                />
                              ) : null}

                              {message.approvals && message.approvals.length > 0 ? (
                                <div className="grid gap-1.5">
                                  {message.approvals.map((ap) => (
                                    <ApprovalCard
                                      key={ap.id}
                                      approval={ap}
                                      conversationId={realConvId(currentConvKey) ?? ""}
                                      onResolved={(next) => {
                                        patchAssistantMessage(currentConvKey, message.id, (m) => ({
                                          ...m,
                                          approvals: (m.approvals ?? []).map((a) =>
                                            a.id === next.id ? next : a,
                                          ),
                                        }));
                                      }}
                                      onModelSwitched={(model) => setSelectedModel(model)}
                                      onSwitchAndRetry={() => retryLastUserMessage()}
                                    />
                                  ))}
                                </div>
                              ) : null}

                              {message.memoryProposals && message.memoryProposals.length > 0 ? (
                                <div className="grid gap-1.5">
                                  {message.memoryProposals.map((mp) => (
                                    <MemoryProposalCard
                                      key={mp.id}
                                      proposal={mp}
                                      onResolved={(next) => {
                                        patchAssistantMessage(currentConvKey, message.id, (m) => ({
                                          ...m,
                                          memoryProposals: (m.memoryProposals ?? []).map((p) =>
                                            p.id === next.id ? next : p,
                                          ),
                                        }));
                                        if (next.status === "saved") {
                                          void loadMemories();
                                        }
                                      }}
                                    />
                                  ))}
                                </div>
                              ) : null}

                              {message.state === "done" && message.content ? (
                                <div className="flex items-center gap-3 text-[0.7rem]">
                                  <CopyButton text={message.content} />
                                  {!message.cancelled &&
                                  !message.failed &&
                                  message.id === lastAssistantMessageId &&
                                  !isStreaming ? (
                                    <button
                                      type="button"
                                      className="touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                                      onClick={() => void regenerateLastAssistant()}
                                    >
                                      Regenerate
                                    </button>
                                  ) : null}
                                  {/* Fork the conversation at this (persisted) message into a
                                      new thread (#454). Only persisted messages carry a dbId;
                                      in-flight ones can't be a branch point yet. */}
                                  {message.dbId && !isStreaming ? (
                                    <button
                                      type="button"
                                      className="touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                                      onClick={() => void branchFromMessage(message)}
                                    >
                                      Branch
                                    </button>
                                  ) : null}
                                </div>
                              ) : null}
                            </div>
                          </div>
                          )}
                        </div>
                      </article>
                            );
                          }}
                        </VirtualTranscriptRow>
                      );
                    })}
                  </div>
                  <div ref={streamEndRef} />
                </div>
              )}
            </div>
          </section>
  );
}

// ── User bubble with inline edit ─────────────────────────────────────────
//
// The most-recent user message gets an "Edit" affordance. Editing submits
// a replacement and truncates the prior turn on the server so the assistant
// regenerates from the edit. Older user turns are read-only to keep the
// transcript coherent.

function UserBubble({
  message,
  isLastUser,
  isStreaming,
  editRequestSignal,
  onResend,
}: {
  message: Message;
  isLastUser: boolean;
  isStreaming: boolean;
  // Keyboard `e` shortcut (#306): a bumped nonce asks this bubble to enter edit
  // mode. Only the last user message ever receives a non-zero value. 0 = no
  // request.
  editRequestSignal: number;
  onResend: (edited: string) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(message.content);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);

  // Keep the draft in sync with the canonical message content whenever we're
  // NOT actively editing. This is the classic "reset local state when a prop
  // changes" case. React's recommended fix is to adjust state *during render*
  // (tracking the values that should trigger a reset) rather than in an
  // effect — that avoids the extra render an effect-driven setState costs and
  // keeps it off the synchronous effect phase. We watch both `editing` (so
  // leaving edit mode restores the canonical text) and `message.content` (so
  // an out-of-band update to a not-being-edited message resyncs). In-progress
  // edits are never clobbered because the reset only fires while not editing.
  const [lastSyncedContent, setLastSyncedContent] = useState(message.content);
  const [wasEditing, setWasEditing] = useState(editing);
  if (!editing && (message.content !== lastSyncedContent || wasEditing)) {
    setDraft(message.content);
    setLastSyncedContent(message.content);
    setWasEditing(false);
  } else if (editing !== wasEditing) {
    setWasEditing(editing);
  }

  // On enter into edit mode: focus + cursor at end so a long message
  // is ready to extend, not select-all. Width/height auto-sizing is
  // handled below by the invisible text mirror in the edit-mode JSX
  // — no JS height math needed.
  useEffect(() => {
    if (!editing) return;
    const el = textareaRef.current;
    if (!el) return;
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }, [editing]);

  const startEdit = () => setEditing(true);

  // React to the keyboard `e` shortcut: enter edit mode when the parent bumps
  // editRequestSignal. Gated the same way the Edit button is (last user turn,
  // not mid-stream). We compare against the last-seen nonce *during render* —
  // the same "reset state when a prop changes" pattern used for draft above —
  // so it fires once per request without a setState-in-effect cascade.
  const [seenEditSignal, setSeenEditSignal] = useState(editRequestSignal);
  if (editRequestSignal !== seenEditSignal) {
    setSeenEditSignal(editRequestSignal);
    if (editRequestSignal > 0 && isLastUser && !isStreaming && !editing) {
      setEditing(true);
    }
  }

  const cancelEdit = () => {
    setEditing(false);
    setDraft(message.content);
  };
  const saveEdit = () => {
    if (!draft.trim() || draft === message.content) {
      cancelEdit();
      return;
    }
    setEditing(false);
    onResend(draft);
  };

  if (editing) {
    return (
      // items-end keeps both the bubble AND the action row right-anchored
      // inside the parent's max-w-[88%] cap, matching the idle bubble's
      // right alignment.
      <div className="flex flex-col items-end gap-1.5">
        {/* Bubble. inline-grid + invisible text mirror sizes the wrapper */}
        {/* to whichever child is wider/taller — so the bubble hugs its  */}
        {/* content the way an idle bubble does, but with a live         */}
        {/* React-controlled textarea overlaid on top. As you type, the  */}
        {/* mirror's text grows and the wrapper grows with it; the       */}
        {/* textarea fills the same grid cell so the user only ever sees */}
        {/* one bubble.                                                  */}
        <div className="relative inline-grid min-w-0 max-w-full rounded-[1.1rem] bg-[var(--color-overlay-soft)] px-3 py-2.5 text-[0.875rem] leading-[1.55] ring-1 ring-[var(--color-accent)] sm:rounded-[1.25rem] sm:px-4 sm:py-3 sm:text-[0.9375rem] sm:leading-[1.65]">
          <span
            aria-hidden="true"
            // Trailing space matters: when the draft ends with '\n', the
            // mirror would otherwise collapse its last line and the
            // textarea's cursor would render outside the bubble.
            // overflow-wrap:anywhere mirrors the idle bubble's wrap rule.
            className="invisible whitespace-pre-wrap [overflow-wrap:anywhere] [grid-area:1/1]"
          >
            {draft + " "}
          </span>
          <textarea
            ref={textareaRef}
            // m-0 + border-0 + p-0 strip the textarea's UA defaults so it
            // aligns pixel-for-pixel with the mirror. Padding/font/leading
            // live on the wrapper; both children inherit them.
            className="m-0 block w-full resize-none border-0 bg-transparent p-0 leading-[inherit] text-inherit text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] [grid-area:1/1]"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                saveEdit();
              } else if (e.key === "Escape") {
                e.preventDefault();
                cancelEdit();
              }
            }}
          />
        </div>
        <div className="flex items-center gap-2">
          {/* Keyboard hint — desktop only; phones don't have these  */}
          {/* keys and the row is tight on width.                    */}
          <span className="hidden text-[0.65rem] text-[var(--color-text-muted)] sm:inline">
            <kbd className="font-mono">↵</kbd> to send
            <span className="opacity-60"> · </span>
            <kbd className="font-mono">Esc</kbd> to cancel
          </span>
          <button
            type="button"
            className="text-[0.72rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
            onClick={cancelEdit}
          >
            Cancel
          </button>
          <button
            type="button"
            className="inline-flex items-center gap-1 rounded-full bg-[var(--color-text-primary)] px-3 py-1 text-[0.72rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:cursor-not-allowed disabled:opacity-30"
            disabled={!draft.trim() || draft === message.content}
            onClick={saveEdit}
          >
            Resend
          </button>
        </div>
      </div>
    );
  }

  return (
    <div>
      {/* [overflow-wrap:anywhere] (not Tailwind's break-words = break-word)
          lets the bubble's intrinsic min-width drop to a single character,
          so a pasted long URL/token wraps inside the max-w-[88%] cap
          instead of pushing the bubble past the chat column on mobile. */}
      <div className="min-w-0 [overflow-wrap:anywhere] rounded-[1.1rem] bg-[var(--color-overlay-soft)] px-3 py-2.5 text-[0.875rem] leading-[1.55] text-[var(--color-text-primary)] sm:rounded-[1.25rem] sm:px-4 sm:py-3 sm:text-[0.9375rem] sm:leading-[1.65]">
        <MessageMarkdown content={message.content} />
      </div>
      {isLastUser && !isStreaming ? (
        // "Edit" text action in an in-flow footer row, mirroring the
        // assistant side's Copy / Regenerate footer (same text-[0.7rem]
        // + touch-target style). justify-end keeps it right-anchored
        // under the right-aligned user bubble. Being in normal flow —
        // not absolute — is deliberate: the old absolute pencil drifted
        // over the next message on mobile because its offset competed
        // with the inter-message gap. A flow element just sits under its
        // own bubble at every viewport. The 40px touch-target min-height
        // gives a reliable tap area on phones.
        // aria-label is the bare verb "Edit" so the accessible name
        // stays a single canonical token (e2e tests + screen readers).
        <div className="mt-1.5 flex items-center justify-end text-[0.7rem]">
          <button
            type="button"
            aria-label="Edit"
            title="Edit message"
            className="touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            onClick={startEdit}
          >
            Edit
          </button>
        </div>
      ) : null}
    </div>
  );
}

// ── Reasoning block ──────────────────────────────────────────────────────
//
// Renders the model's streamed reasoning. Only mounted when the global
// "Show details" toggle (showStats) is on — so the user opted into seeing
// the thinking, and we default to expanded. Users can still click the
// header to collapse a noisy block manually.

function ReasoningBlock({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(true);

  return (
    <div className="rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_68%,transparent)] px-3 py-2 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]">
      <button
        type="button"
        className="flex w-full items-center justify-between gap-3 text-left"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
      >
        <span className="text-[0.68rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
          Thinking
        </span>
        <span className="text-[0.68rem] text-[var(--color-text-muted)]">
          {expanded ? "Hide" : "Show"}
        </span>
      </button>
      {expanded ? (
        <div className="mt-2">
          <MessageMarkdown content={text} />
        </div>
      ) : null}
    </div>
  );
}

// SummarizeProgressCard renders during compaction so the user sees
// the model's summary materialize in real-time instead of staring at
// a frozen spinner. Compaction can take 30-60s on a long chat (it's
// a single large completion across the whole history) — without a
// visible signal of progress, the wait reads as "broken UI".
//
// Three pieces of state-of-mind UX:
//   - Animated logo + label so it's obvious the system is working.
//   - Elapsed-time counter ticking up so the user can gauge how long
//     they've been waiting (and see that progress is actually happening
//     vs. a hung request).
//   - The streaming summary text appears in the body of the card,
//     same as a normal assistant message — turns the wait into "the
//     model is writing, watch it write".
function SummarizeProgressCard({
  startedAt,
  streamingText,
}: {
  startedAt: number | null;
  streamingText: string;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (startedAt === null) return;
    const id = window.setInterval(() => setNow(Date.now()), 500);
    return () => window.clearInterval(id);
  }, [startedAt]);
  const elapsedSeconds = startedAt ? Math.max(0, Math.floor((now - startedAt) / 1000)) : 0;

  return (
    <div className="rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_72%,transparent)] px-4 py-3 text-[var(--color-text-primary)]">
      <div className="flex items-center gap-3">
        <LoadingLogo size={22} />
        <div className="flex min-w-0 flex-1 flex-col">
          <span className="text-[0.78rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            Compacting conversation
          </span>
          <span className="text-[0.72rem] text-[var(--color-text-muted)]">
            Replacing earlier turns with a short summary so the next turn fits in
            the model&apos;s context window.
          </span>
        </div>
        <span
          className="shrink-0 rounded-full bg-[var(--color-overlay-strong)] px-2 py-0.5 text-[0.7rem] tabular-nums text-[var(--color-text-secondary)]"
          aria-live="polite"
        >
          {elapsedSeconds}s
        </span>
      </div>
      {streamingText ? (
        <div className="mt-3 grid gap-2 border-t border-[var(--color-border)] pt-3 text-[0.85rem] leading-[1.55] text-[var(--color-text-primary)]">
          <MessageMarkdown content={streamingText} />
        </div>
      ) : null}
    </div>
  );
}
