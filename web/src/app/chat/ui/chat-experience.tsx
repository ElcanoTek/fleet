"use client";

// NOTE (lint hygiene): this large component used to disable three React
// Compiler rules file-wide (react-hooks/set-state-in-effect,
// react-hooks/purity, react-hooks/immutability). Those disables hid real
// hook smells, so they were removed and the underlying patterns fixed
// instead: wall-clock reads go through the module-level `nowMs()` helper
// (purity); the three bootstrap mount effects were hoisted below the
// callbacks they call and call mutually-recursive callbacks through
// latest-refs (immutability + exhaustive-deps); and the few effect-phase
// setStates were moved off the synchronous render path (lazy init,
// derive-in-render, handler-side resets, or a deferred microtask). Keep this
// component clean — prefer those patterns over re-adding a rule disable.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { deriveConversationTitle } from "@/app/lib/title";
import {
  ADVANCED_MODEL,
  ADVANCED_MODEL_LABEL,
  DEFAULT_MODEL,
  DEFAULT_MODEL_LABEL,
} from "@/app/lib/modelAliases";
import {
  computeContextUsage,
  type ContextUsage,
} from "@/app/lib/contextUsage";
import { parseSseChunk } from "@/app/lib/sse";
import { decideSpreadsheetNudge } from "@/app/lib/spreadsheetNudge";
import { SearchBar } from "./SearchBar";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { filterConversations } from "./conversationOrganization";
import {
  useKeyboardShortcuts,
  type KeyboardShortcut,
} from "@/app/shared/hooks/useKeyboardShortcuts";
import {
  KeyboardShortcutsOverlay,
  type ShortcutHelpGroup,
} from "@/app/shared/ui/KeyboardShortcutsOverlay";
import {
  historyToMessages,
  type Approval,
  type HistoryEntry,
  type MemoryProposal,
  type Message,
} from "./history";
import { PENDING_CONV_KEY } from "./workspaceHref";
import { Icon } from "./Icon";
import { ProjectsModal } from "./ProjectsModal";
import { MemoryGraphView } from "./MemoryGraphView";
import {
  ConversationTotalsChip,
  type PendingAttachment,
} from "./ChatChips";
import { ConversationSidebar } from "./ConversationSidebar";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { BulkDeleteConfirmModal } from "./BulkDeleteConfirmModal";
import { Composer } from "./Composer";
import type { SkillInfo } from "./skillSlash";
import { ChatTranscript } from "./ChatTranscript";
import { usePerConvComposerState } from "./usePerConvComposerState";
import { useTurnStreamState } from "./useTurnStreamState";
import { useTurnStream, type TurnStreamDeps } from "./useTurnStream";
// The assistant markdown renderer lives in its own module now. Re-export
// the public API so existing import paths — including the markdown unit
// tests that import { autoFenceRawHtmlDocument, renderAssistantContent }
// from "./chat-experience" — keep resolving unchanged.
import { autoFenceRawHtmlDocument, renderAssistantContent } from "./AssistantContent";
export { autoFenceRawHtmlDocument, renderAssistantContent };

// Wall-clock read, isolated in a module-level helper. The async stream
// handlers below run during a render pass (the React Compiler's lint rules
// treat their bodies as render-reachable), so a bare `Date.now()` there
// trips react-hooks/purity. Routing the read through a named module-scope
// function keeps the impurity out of the component body without changing
// behavior — these timestamps are only used for elapsed-time math and as
// monotonic-ish local message ids, never as render-affecting derived state.
const nowMs = (): number => Date.now();

// ── types ────────────────────────────────────────────────────────────────
//
// Core message types live in ./history.ts so they can be unit-tested under
// vitest without pulling in React/Next.js. We re-use them here.

export type ConversationSummary = {
  id: string;
  title: string;
  persona: string;
  model: string;
  pinned: boolean;
  updated_at: number;
  // Lockdown is set once at conversation creation. When true, the
  // backend forces a per-turn container sandbox and restricts model
  // slugs to the server's lockdown allow-list. Drives the lock-icon
  // badge in the sidebar + chat header and filters the model picker.
  lockdown?: boolean;
  // archived_at is null for active conversations and a unix timestamp for
  // archived ones (#282). Archived conversations live in the collapsed
  // "Archived" sidebar section, not the main list.
  archived_at?: number | null;
  // folder is the single flat bucket this conversation is filed under (#279).
  // Empty/undefined = unfiled. Folders are derived from these values in the
  // rail — there is no folders table. Filing a conversation auto-pins it.
  folder?: string;
  // labels is the conversation's tag set (#279) — up to 10, 32 chars each,
  // colored by name-hash. Undefined/empty = unlabeled.
  labels?: string[];
  // share_token is the public read-only share token (#226). Empty/undefined =
  // not shared; non-empty = a 🔗 badge shows in the sidebar and the kebab offers
  // Copy link / Unshare. The owner's own GET /conversations carries it.
  share_token?: string;
};

export type ServerConfig = {
  // lockdownAvailable: lockdown UI should be exposed (sandbox image
  // configured on the server). When false, no lockdown button at all.
  lockdownAvailable: boolean;
  // lockdownOnly: every chat is forcibly lockdown — hide the regular
  // "+" button, only show the lockdown one. For sensitive deploys.
  lockdownOnly: boolean;
  lockdownAllowedModels: string[];
};

export type PendingDeleteConversation = {
  id: string;
  title: string;
};

type PersonasResponse = {
  personas: string[];
  default: string;
};

export type RankedModel = {
  slug: string;
  name: string;
  // Total context window from the OpenRouter catalog. Optional —
  // /api/model-rankings does not include it (its response is the raw
  // ranking page, no catalog join), but /api/model-catalog does. Used
  // by the context-usage indicator next to total cost when the user
  // has Show details on.
  contextLength?: number;
  // Unix timestamp (seconds) the model was first listed on OpenRouter.
  // Drives the "✨ new" pill in the picker — entries within
  // NEW_MODEL_WINDOW_DAYS get the badge.
  created?: number;
};

// Optional MCP servers the user can toggle on per-conversation. The catalog
// (name, description, tool count, enabled) comes from the chat-server and
// is fetched when a conversation is loaded. Non-optional servers are not
// represented here — they're always on.
// Exported (and hoisted to module scope) so the extracted Composer can type
// its mcpServers prop without re-declaring the shape.
export type MCPServerInfo = {
  name: string;
  /** Prettified label rendered in the picker. Falls back to `name`. */
  display_name?: string;
  description: string;
  tools: string[];
  tool_count: number;
  enabled: boolean;
  /** Server is in beta — UI renders a "BETA" badge next to display_name. */
  beta?: boolean;
  /** Server starts ON for a brand-new chat (the user can still turn it
   *  off). Used to seed the empty-state toggle when resetting to a fresh
   *  conversation. */
  enabled_by_default?: boolean;
};

// NEW_MODEL_WINDOW_DAYS + isNewlyReleased moved into ./Composer alongside
// their only caller (the model-picker rows) during the #169 decomposition.

type UserMemory = {
  id: string;
  content: string;
  source: "manual" | "chat" | string;
  kind?: string;
  origin?: string;
  conversation_id?: string;
  pinned?: boolean;
  valid_from?: number;
  valid_to?: number;
  learned_at?: number;
  retired_at?: number;
  retired_by?: string;
  created_at: number;
  updated_at: number;
};

const MEMORY_KINDS = ["fact", "preference", "identity", "constraint", "context"] as const;

// ── streaming helpers ────────────────────────────────────────────────────
// minimumThinkingMs / streamIdleTimeoutMs moved into ./useTurnStream alongside
// the turn loop that is their only consumer.

// Composer textarea height cap (~8 lines at the composer's font/leading).
// This is the single source of truth: the textarea's Tailwind classes
// intentionally omit any `max-h-*` so this JS clamp is the only one that
// fires. `overflow-y-auto` on the element lets it scroll internally once
// capped. See the autosize `useEffect` near `promptRef`.
const MAX_COMPOSER_HEIGHT_PX = 200;

// shortcutHelpGroups is the single source of truth for the "?" help overlay
// (#306). It documents only the shortcuts the shell actually wires through
// useKeyboardShortcuts below — keep the two in step. Chips render
// platform-aware (⌘ on macOS, Ctrl elsewhere) in the overlay.
const shortcutHelpGroups: ShortcutHelpGroup[] = [
  {
    title: "Global",
    entries: [
      { chips: [{ mod: true }, { label: "K" }], description: "Open search" },
      { chips: [{ mod: true }, { label: "N" }], description: "New conversation" },
      { chips: [{ mod: true }, { label: "J" }], description: "Focus the message composer" },
      { chips: [{ label: "?" }], description: "Show this keyboard-shortcut help" },
      { chips: [{ label: "Esc" }], description: "Close search, help, or the sidebar" },
    ],
  },
  {
    title: "Composer",
    entries: [
      { chips: [{ label: "Enter" }], description: "Send the message" },
      { chips: [{ mod: true }, { label: "Enter" }], description: "Send the message" },
      { chips: [{ shift: true }, { label: "Enter" }], description: "Insert a newline" },
    ],
  },
];

// ── component ────────────────────────────────────────────────────────────

// PENDING_CONV_KEY is the sentinel for messages that belong to a brand-new
// chat whose server id we haven't received yet. submitPrompt drops the
// user/assistant placeholder pair under this key; streamTurn renames it
// to the real conversation id when the "conversation" SSE event arrives.
// The constant lives in ./workspaceHref.ts so the rewrite helpers can
// share it with no React dependency.

export function ChatExperience() {
  // Keep messages keyed by conversation id (plus the PENDING sentinel
  // for brand-new chats). This lets users navigate between chats during
  // an in-flight stream without losing the streaming UI state — the
  // stream events keep landing in the originating conv's slot whether
  // it's currently displayed or not.
  const [messagesByConv, setMessagesByConv] = useState<Record<string, Message[]>>({});
  // Per-conversation composer state — drafts, queued attachments, attachment
  // errors, and in-flight upload marks — lives in usePerConvComposerState,
  // keyed by currentConvKey (real conv id or the PENDING sentinel for the
  // empty new-chat view). The hook is instantiated below, once
  // currentConvKey is in scope. See ./usePerConvComposerState.
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // searchOpen gates the Cmd/Ctrl+K full-text search palette (#308).
  const [searchOpen, setSearchOpen] = useState(false);
  // shortcutsOpen gates the "?" keyboard-shortcut help overlay (#306).
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  // Theme bootstrap + toggle are owned by the shared shell: the <ThemeToggle/>
  // in this view's header self-manages light/dark via useTheme, the same way
  // the orchestrator header and login card do.
  // showStats gates the whole per-turn "details" block: the cost /
  // latency / tokens / model chip, the conversation-wide totals chip,
  // AND the execution trail (tool pills + python stream output). One
  // switch for every technical surface so non-developers get a clean
  // chat and power users can flip everything on at once. Persisted to
  // localStorage so the preference survives reloads.
  const [showStats, setShowStats] = useState(false);
  // Per-conversation turn/SSE transport state — the streaming set (sidebar
  // "working" dots) plus abort controllers, attached/reattach tracking, and
  // the SSE dedup counters — lives in useTurnStreamState, keyed by conv
  // slot. Instantiated below once currentConvKey is in scope. See
  // ./useTurnStreamState.
  const [crossfadingMessageIds, setCrossfadingMessageIds] = useState<number[]>([]);
  const [userEmail, setUserEmail] = useState("");
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  // Archived conversations (#282): fetched separately, shown in a collapsed
  // "Archived" section at the bottom of the sidebar.
  const [archivedConversations, setArchivedConversations] = useState<ConversationSummary[]>([]);
  const [showArchived, setShowArchived] = useState(false);
  const [activeConversationId, setActiveConversationId] = useState<string | null>(null);
  // The slot identifier the rest of the UI hangs off of: the active
  // conv id when one is loaded, the PENDING sentinel when the user is
  // on the empty new-chat view, or a per-submission pending key during
  // the brief window between submit and the server's "conversation"
  // SSE event. messagesByConv plus the per-conv composer and turn-stream
  // state (in usePerConvComposerState / useTurnStreamState below) all key
  // on this string.
  const currentConvKey = activeConversationId ?? PENDING_CONV_KEY;
  // Per-conversation turn/SSE transport state, keyed by conv slot. The
  // derived `isStreaming` is true when the conv the user is currently
  // looking at has a turn in flight (drives composer disabled states, Stop
  // button visibility, auto-scroll). Other conversations may stream
  // simultaneously — the sidebar reads `streamingConvs` for its dots. See
  // ./useTurnStreamState. `renameStreamingKey` is intentionally not pulled
  // out: its only caller is promoteStreamKey, inside the hook.
  const {
    streamingConvs,
    streamingConvsRef,
    isStreaming,
    markConvStreaming,
    markConvIdle,
    abortControllersRef,
    attachedConvIdsRef,
    lastEventIdByConvRef,
    currentTurnIdByConvRef,
    reattachInFlightRef,
    promoteStreamKey,
  } = useTurnStreamState(currentConvKey);
  // Per-conversation composer state — drafts, queued attachments, attachment
  // errors, in-flight upload marks — keyed by currentConvKey. The
  // dispatch-compatible setters (setPrompt / setPendingAttachments /
  // setAttachmentError) capture currentConvKey at *render* time, so an async
  // submit in conv A keeps writing to A's slot even after the user navigates
  // to conv B. See ./usePerConvComposerState.
  const {
    prompt,
    pendingAttachments,
    attachmentError,
    isUploadingAttachments,
    setPrompt,
    setPendingAttachments,
    setAttachmentError,
    setPromptForKey,
    setPendingAttachmentsForKey,
    setAttachmentErrorForKey,
    markConvUploading,
    markConvUploadDone,
    getPendingAttachmentsForKey,
    promoteComposerKey,
  } = usePerConvComposerState(currentConvKey);
  // Per-submission pending keys: every brand-new chat submission gets
  // its own `__pending__:<n>` key so two new chats in flight at the
  // same time can't collide on the singleton PENDING_CONV_KEY. The
  // conversation event renames the per-submission key → real conv id
  // when the server confirms the slot. The PENDING_CONV_KEY singleton
  // is reserved for the *empty* new-chat view's composer state.
  const pendingKeyCounterRef = useRef(0);
  const nextPendingKey = (): string => {
    pendingKeyCounterRef.current += 1;
    return `${PENDING_CONV_KEY}:${pendingKeyCounterRef.current}`;
  };
  const isPendingKey = (key: string | null): boolean =>
    key === null ||
    key === PENDING_CONV_KEY ||
    key.startsWith(`${PENDING_CONV_KEY}:`);
  const realConvId = (key: string | null): string | null =>
    key && !isPendingKey(key) ? key : null;
  const [isLoadingHistory, setIsLoadingHistory] = useState(true);
  const [pendingDeleteConversation, setPendingDeleteConversation] =
    useState<PendingDeleteConversation | null>(null);
  // Header title click-to-edit. Holds the draft string while the input is
  // open; null means the static label is shown.
  const [renamingTitleDraft, setRenamingTitleDraft] = useState<string | null>(null);
  const [isSavingTitle, setIsSavingTitle] = useState(false);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [confirmSummarize, setConfirmSummarize] = useState(false);
  // Multi-select bulk operations (#279). selectedIds is the set of checked
  // conversation rows in the sidebar. bulkDeleteConfirm opens the 3-second
  // countdown confirm modal for a targeted bulk delete of the selection.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [bulkDeleteConfirm, setBulkDeleteConfirm] = useState(false);
  const [showJumpToLatest, setShowJumpToLatest] = useState(false);
  const [personas, setPersonas] = useState<string[]>([]);
  const [selectedPersona, setSelectedPersona] = useState<string>("");
  // Which empty-state protocol pill the user has opened into its form/intake
  // panel (null = show the card grid). Only meaningful on the empty new-chat
  // view; reset whenever we return to a clean slate.
  const [activePillId, setActivePillId] = useState<string | null>(null);
  // Runtime client config: branding strings + the empty-state quick-start
  // cards, fetched from the member-gated /api/client-config so the UI is
  // client-agnostic. Falls back to neutral defaults on error / while loading.
  const { branding, pills } = useClientConfig();
  // selectedModel is the OpenRouter slug for the active conversation. Empty
  // means "use the server-configured primary." It can be edited mid-chat;
  // submitPrompt forwards the current value with every turn so the backend
  // persists changes against the conversation row. The two blessed slugs
  // (DEFAULT_MODEL = fast tier, ADVANCED_MODEL = strong tier) live in
  // ../lib/modelAliases so other surfaces (nudge banners, tests) share a
  // single source of truth.
  const [selectedModel, setSelectedModel] = useState<string>(DEFAULT_MODEL);
  const [rankedModels, setRankedModels] = useState<RankedModel[]>([]);
  const [catalogModels, setCatalogModels] = useState<RankedModel[]>([]);
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [personaPickerOpen, setPersonaPickerOpen] = useState(false);
  const [modelSearchQuery, setModelSearchQuery] = useState<string>("");
  const [isLoadingRankedModels, setIsLoadingRankedModels] = useState(false);
  const [isLoadingCatalog, setIsLoadingCatalog] = useState(false);
  // modelError is set when the custom slug in the model input is rejected
  // by /api/model-check (currently: completion price > $30/M). When set,
  // submitPrompt refuses to send and the composer shows the error.
  const [modelError, setModelError] = useState<{ message: string; modelsUrl: string } | null>(
    null,
  );
  // Optional MCP servers the user can toggle on per-conversation. The
  // MCPServerInfo shape is declared at module scope (and exported) so the
  // extracted Composer can type its prop against it.
  const [mcpServers, setMcpServers] = useState<MCPServerInfo[]>([]);
  const [mcpPickerOpen, setMcpPickerOpen] = useState(false);
  // Bundle skill roster for the composer "/" autocomplete (#513). Fetched
  // once at startup — the roster is bundle-owned and static for the session.
  const [skills, setSkills] = useState<SkillInfo[]>([]);
  // Server-exposed capability flags. Fetched once on mount from
  // /api/server-config. Drives the lockdown affordance: when
  // lockdownAvailable is false the +button stays a plain "+"
  // (matches the UI-as-it-is-now contract for operators who haven't
  // opted into lockdown opt-in mode).
  const [serverConfig, setServerConfig] = useState<ServerConfig>({
    lockdownAvailable: false,
    lockdownOnly: false,
    lockdownAllowedModels: [],
  });
  // pendingLockdown is set when the user clicks "New lockdown chat"
  // and cleared once the conversation is actually created. The flag
  // rides along on the first /api/chat POST as `lockdown: true`.
  const [pendingLockdown, setPendingLockdown] = useState(false);
  // activeConversation tracks the currently-active conversation
  // record (or null for a brand-new pending chat). Used so the chat
  // header can render the lockdown badge without re-walking the
  // conversation list.
  // Search both lists: a conversation archived while it is the open one (#282)
  // moves out of `conversations` into `archivedConversations`, but the chat view
  // stays on it — so the header title, lockdown badge, and tab title must still
  // resolve it rather than going blank.
  const activeConversation = useMemo(
    () =>
      conversations.find((c) => c.id === activeConversationId) ??
      archivedConversations.find((c) => c.id === activeConversationId) ??
      null,
    [conversations, archivedConversations, activeConversationId],
  );
  // Lockdown state for the current view:
  //   - Active conversation flagged lockdown → use that
  //   - Pending new chat that the user clicked "+ Lockdown" for → true
  //   - LockdownOnly server mode → every new chat is implicitly
  //     lockdown (header badge + filtered model picker + the badge in
  //     the empty state all light up by default)
  const isLockdown =
    activeConversation?.lockdown === true ||
    (isPendingKey(activeConversationId) &&
      (pendingLockdown || serverConfig.lockdownOnly));
  const [isLoadingMcpServers, setIsLoadingMcpServers] = useState(false);
  const [memories, setMemories] = useState<UserMemory[]>([]);
  const [memoryManagerOpen, setMemoryManagerOpen] = useState(false);
  // Memory-manager tab (#523): the flat record list (default) or the derived
  // knowledge-graph view.
  const [memoryView, setMemoryView] = useState<"list" | "graph">("list");
  const [projectsOpen, setProjectsOpen] = useState(false);
  const [memoryDraft, setMemoryDraft] = useState("");
  const [memoryKindDraft, setMemoryKindDraft] = useState<string>("fact");
  const [editingMemoryId, setEditingMemoryId] = useState<string | null>(null);
  const [memoryError, setMemoryError] = useState<string | null>(null);
  const [isLoadingMemories, setIsLoadingMemories] = useState(false);
  const [isSavingMemory, setIsSavingMemory] = useState(false);
  const [sidebarQuery, setSidebarQuery] = useState("");
  // Rail organization filters (#258/#279): a single active folder and a set of
  // labels (AND). Driving the sectioned-vs-filtered view in the rail.
  const [filterFolder, setFilterFolder] = useState<string | null>(null);
  const [filterLabels, setFilterLabels] = useState<string[]>([]);
  // pendingAttachments holds files the user has picked but not yet sent.
  // We upload them to the server on submit, get back metadata with a
  // server-trusted path, and forward that in the /api/chat body. The
  // backing per-conv records live in usePerConvComposerState (destructured
  // above as pendingAttachments / isUploadingAttachments).
  const [isDraggingOver, setIsDraggingOver] = useState(false);
  const dragCounterRef = useRef(0);
  // spreadsheetNudgeDismissed gates the "switch to advanced model"
  // banner that appears when a heavy .xlsx is queued on the default
  // model. Cleared automatically when the attachment list empties so
  // the next upload re-arms the suggestion.
  const [spreadsheetNudgeDismissed, setSpreadsheetNudgeDismissed] = useState(false);
  // Compaction state. isSummarizing gates the Summarize button (and
  // disables it while the network call is in flight). summarizeStream
  // accumulates the streaming summary text as the model generates it
  // — feeds the progress card so the user sees the summary materialize
  // instead of staring at a frozen spinner for 30-60s. summaryExpanded
  // reveals pre-summary messages — collapsed by default so the
  // freshly-compacted chat reads as a clean phase boundary; reset on
  // conversation switch.
  const [isSummarizing, setIsSummarizing] = useState(false);
  const [summarizeError, setSummarizeError] = useState<string | null>(null);
  const [summarizeStream, setSummarizeStream] = useState("");
  const [summarizeStartedAt, setSummarizeStartedAt] = useState<number | null>(null);
  const [summaryExpanded, setSummaryExpanded] = useState(false);
  const spreadsheetNudge = useMemo(
    () =>
      decideSpreadsheetNudge({
        attachments: pendingAttachments.map((a) => ({ name: a.name, size: a.size })),
        selectedModel,
        dismissed: spreadsheetNudgeDismissed,
      }),
    [pendingAttachments, selectedModel, spreadsheetNudgeDismissed],
  );
  const promptRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const streamEndRef = useRef<HTMLDivElement | null>(null);
  const conversationRef = useRef<HTMLElement | null>(null);
  // When a send_email approval is appended via SSE, the new card may land
  // below the fold (especially after a preview_email card has expanded an
  // iframe and pushed the send card off-screen). Queue its id so the
  // scroll effect below brings it into view minimally on the next render.
  const pendingApprovalScrollRef = useRef<string | null>(null);
  const pendingHistoryScrollRef = useRef<string | null>(null);
  const searchRef = useRef<HTMLInputElement | null>(null);
  const modelPickerRef = useRef<HTMLDivElement | null>(null);
  // Direct ref on the model <input> so an option-pick handler can blur
  // it explicitly. Without an explicit blur the input keeps focus
  // (because option onMouseDown preventDefault is what stops the picker
  // from re-opening), and on touch that means the keyboard stays up
  // and the wrapper's `focus-within` keeps it expanded long after the
  // user is done picking.
  const modelInputRef = useRef<HTMLInputElement | null>(null);
  const personaPickerRef = useRef<HTMLDivElement | null>(null);
  const mcpPickerRef = useRef<HTMLDivElement | null>(null);
  const fadeTimeoutsRef = useRef<number[]>([]);
  const messagesByConvRef = useRef<Record<string, Message[]>>({});
  const activeConversationIdRef = useRef<string | null>(null);
  // Cache-bust drift flag. Set when /api/version reports a new build id.
  // We never reload the page automatically — instead we surface an
  // "Update available" button in the sidebar so the user chooses when
  // to refresh. State (not a ref) so the sidebar re-renders on change.
  const [updateAvailable, setUpdateAvailable] = useState(false);

  useEffect(() => {
    messagesByConvRef.current = messagesByConv;
  }, [messagesByConv]);

  useEffect(() => {
    activeConversationIdRef.current = activeConversationId;
  }, [activeConversationId]);

  // Keyboard shortcuts are registered declaratively through the shared
  // useKeyboardShortcuts hook below (search the comment "shortcut catalog"),
  // placed after the callbacks they invoke (clearConversation etc.) are in
  // scope. The two ad-hoc window keydown listeners that used to live here — the
  // #308 search palette binding and an older sidebar-search focus binding — were
  // folded into that one registration so every shortcut is discoverable in the
  // "?" help overlay (#306).

  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (modelPickerRef.current?.contains(target)) return;
      setModelPickerOpen(false);
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => window.removeEventListener("mousedown", onPointerDown);
  }, []);

  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (personaPickerRef.current?.contains(target)) return;
      setPersonaPickerOpen(false);
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => window.removeEventListener("mousedown", onPointerDown);
  }, []);

  // Close the MCP Tools picker on any click outside its container. Separate
  // effect from the model-picker handler so each picker has its own ref
  // check (both can be open at once in principle, though the UI nudges
  // users to one at a time).
  useEffect(() => {
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (mcpPickerRef.current?.contains(target)) return;
      setMcpPickerOpen(false);
    };
    window.addEventListener("mousedown", onPointerDown);
    return () => window.removeEventListener("mousedown", onPointerDown);
  }, []);

  // ── per-conversation message helpers ────────────────────────────────────

  // setConvMessages writes the array (or applies an updater) for ONE
  // conversation's slot, leaving every other slot untouched. The state
  // setter immediately mirrors into messagesByConvRef so subsequent
  // closure reads (in streamTurn callbacks etc.) see the fresh value
  // before the next render.
  const setConvMessages = (
    convId: string,
    updater: Message[] | ((prev: Message[]) => Message[]),
  ) => {
    setMessagesByConv((prev) => {
      const cur = prev[convId] ?? [];
      const next =
        typeof updater === "function"
          ? (updater as (p: Message[]) => Message[])(cur)
          : updater;
      const merged = { ...prev, [convId]: next };
      messagesByConvRef.current = merged;
      return merged;
    });
  };

  // getConvMessages is a closure-safe read for callers that need the
  // CURRENT value (post-recent-setConvMessages) without going through
  // React state.
  const getConvMessages = (convId: string): Message[] =>
    messagesByConvRef.current[convId] ?? [];

  // renameConvKey moves a slot's array from one key to another. Used to
  // promote PENDING_CONV_KEY → the real conversation id once the server
  // emits the "conversation" event.
  const renameConvKey = (oldKey: string, newKey: string) => {
    setMessagesByConv((prev) => {
      if (oldKey === newKey || !(oldKey in prev)) return prev;
      const next = { ...prev };
      next[newKey] = next[oldKey];
      delete next[oldKey];
      messagesByConvRef.current = next;
      return next;
    });
  };

  // clearConvSlot drops a single conversation's messages from memory.
  // Called by the Clear button and by deleteConversationById so
  // long-lived sessions don't accumulate slots forever.
  const clearConvSlot = (convId: string) => {
    setMessagesByConv((prev) => {
      if (!(convId in prev)) return prev;
      const next = { ...prev };
      delete next[convId];
      messagesByConvRef.current = next;
      return next;
    });
  };

  // What's currently visible? Active conversation's messages, or the
  // PENDING slot if we're in the middle of submitting a brand-new chat.
  // (currentConvKey is declared up top so the per-conv composer derivations
  // can use it.)
  const messages = useMemo(() => {
    return messagesByConv[currentConvKey] ?? [];
  }, [currentConvKey, messagesByConv]);

  // Context-usage signal. Hoisted up here so both the conversation
  // totals chip (in the stats panel) and the composer-side compact
  // ring share one computation. We want the FINAL step's input size
  // of the most recent turn — that's the honest "what's in the
  // model's context window right now" number. The summed
  // promptTokens field would overcount on multi-step agentic turns
  // (9 tool calls × 200k average input per call = 1.8M reported,
  // even though no single step ever sent more than 200k of context).
  // Production incident: that overcounting drove an impossible
  // "200%" context indicator on a heavy fast.io discovery turn
  // (conv 3460d911).
  //
  // Compaction marker (`kind === "summary"`) acts as a hard reset:
  // when we hit one before finding a newer turn, return 0 so the
  // ring clears immediately. The summary marker's OWN prompt_tokens
  // is large (it fed the whole pre-compact history to the model that
  // produced the summary), but that reflects compaction's cost, not
  // the next turn's context fill — the next user turn will only send
  // the summary + new message, dropping prompt_tokens back to low.
  const latestPromptTokens = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      const m = messages[i];
      if (m.kind === "summary") return 0;
      if (m.summary) {
        // Prefer the last-step value when the server provided it
        // (added in the "200%" bugfix). Fall back to the summed
        // promptTokens for older persisted summaries so legacy
        // conversations still get SOME indicator; the renderer's
        // 100%+ clamp keeps that fallback from showing an
        // impossible percentage.
        const lastStep = m.summary.promptTokensLastStep;
        if (typeof lastStep === "number" && lastStep > 0) return lastStep;
        if (m.summary.promptTokens > 0) return m.summary.promptTokens;
      }
    }
    return 0;
  }, [messages]);
  const contextLength = useMemo(() => {
    const slug = selectedModel.trim();
    if (!slug) return undefined;
    return catalogModels.find((m) => m.slug === slug)?.contextLength;
  }, [catalogModels, selectedModel]);
  const contextUsage = useMemo<ContextUsage | null>(
    () =>
      computeContextUsage({
        promptTokens: latestPromptTokens,
        contextLength,
      }),
    [latestPromptTokens, contextLength],
  );

  // One-shot "you should compact" toast. Fires for 3s the moment the
  // active conversation's context first crosses into the danger band
  // (≥90%). Deliberately not a persistent pill — that read as a
  // permanent scold; this is a single brief nudge and then gets out of
  // the way. Switching conversations resets the latch without firing
  // so loading an already-full chat doesn't blast a toast.
  const [compactToastVisible, setCompactToastVisible] = useState(false);
  const compactToastStateRef = useRef<{ severity: string; convId: string | null }>({
    severity: "ok",
    convId: null,
  });
  useEffect(() => {
    const currentSeverity = contextUsage?.severity ?? "ok";
    const currentConvId = activeConversationId;
    const prev = compactToastStateRef.current;
    compactToastStateRef.current = { severity: currentSeverity, convId: currentConvId };

    // Conversation just changed → snapshot the new state without
    // firing. Otherwise opening a long chat that's already at 95%
    // would always pop the toast.
    if (prev.convId !== currentConvId) return;

    if (currentSeverity === "danger" && prev.severity !== "danger") {
      setCompactToastVisible(true);
      const id = window.setTimeout(() => setCompactToastVisible(false), 3000);
      return () => window.clearTimeout(id);
    }
  }, [contextUsage?.severity, activeConversationId]);

  // Theme bootstrap + toggle (read the persisted/system pref, apply
  // data-theme, follow the OS until the user chooses) live in the shared
  // useTheme hook, consumed by the <ThemeToggle/> in this view's header.

  // showStats boot: read once from localStorage. Kept in its own effect
  // (not merged with theme) so SSR hydration doesn't flicker the chip on
  // for a frame before the persisted pref wins. The read happens
  // post-mount (window is only available client-side); the setState is
  // deferred to a microtask so it lands outside the effect's synchronous
  // phase — it patches React state from an external system (localStorage)
  // rather than synchronously cascading a render off the effect body.
  useEffect(() => {
    const stored = window.localStorage.getItem("chat-show-stats");
    if (stored !== "1") return;
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) setShowStats(true);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // NOTE: the catalog-load, visibility-refresh, and initial-load mount
  // effects that used to live here reference callbacks (loadCatalogModels,
  // reattachToConv, refreshConversations, loadConversation,
  // loadMcpServerCatalogPreview) declared further down. They have been
  // moved to just after those declarations (search "mount effects, hoisted
  // below their callback dependencies") so the effect bodies never read a
  // callback before it is declared. Their relative order is preserved.

  // Textarea autosize — clamp at MAX_COMPOSER_HEIGHT_PX (the single source
  // of truth; the textarea's Tailwind classes omit any `max-h-*` so this
  // JS clamp is the only one). `overflow-y-auto` + `transition-[height]`
  // on the element handle the scroll + smooth-growth once capped.
  useEffect(() => {
    const textarea = promptRef.current;
    if (!textarea) return;
    textarea.style.height = "auto";
    textarea.style.height = `${Math.min(textarea.scrollHeight, MAX_COMPOSER_HEIGHT_PX)}px`;
  }, [prompt]);

  const updateJumpToLatestVisibility = () => {
    const scrollParent = conversationRef.current;
    if (!scrollParent) {
      setShowJumpToLatest(false);
      return;
    }
    const distanceFromBottom = scrollParent.scrollHeight - scrollParent.scrollTop - scrollParent.clientHeight;
    setShowJumpToLatest(distanceFromBottom > 240);
  };

  // Auto-scroll behavior
  useEffect(() => {
    const el = streamEndRef.current;
    if (!el) return;
    const scrollParent = conversationRef.current;
    if (!scrollParent) {
      el.scrollIntoView({ block: "end", behavior: isStreaming ? "auto" : "smooth" });
      return;
    }
    const { scrollTop, scrollHeight, clientHeight } = scrollParent;
    const distanceFromBottom = scrollHeight - scrollTop - clientHeight;
    if (isStreaming) {
      if (distanceFromBottom < 240) el.scrollIntoView({ block: "end", behavior: "auto" });
      return;
    }
    if (distanceFromBottom < 160) el.scrollIntoView({ block: "end", behavior: "smooth" });
    updateJumpToLatestVisibility();
  }, [messages, isStreaming]);

  useEffect(() => {
    const conversationId = activeConversationId;
    if (!conversationId || pendingHistoryScrollRef.current !== conversationId || isLoadingHistory) return;

    let frameId = 0;
    let timeoutId: number | null = null;
    const scrollToBottom = () => {
      const scrollParent = conversationRef.current;
      if (!scrollParent) return;
      scrollParent.scrollTop = scrollParent.scrollHeight;
      updateJumpToLatestVisibility();
    };

    frameId = window.requestAnimationFrame(() => {
      scrollToBottom();
      timeoutId = window.setTimeout(scrollToBottom, 80);
    });
    pendingHistoryScrollRef.current = null;

    return () => {
      window.cancelAnimationFrame(frameId);
      if (timeoutId !== null) window.clearTimeout(timeoutId);
    };
  }, [activeConversationId, isLoadingHistory, messages.length]);

  // Bring a freshly-staged send_email approval card into view. The SSE
  // handler queues the approval id; this effect runs after React commits
  // the new card to the DOM and uses block:"nearest" so users already
  // viewing the card aren't re-scrolled.
  useEffect(() => {
    const id = pendingApprovalScrollRef.current;
    if (!id) return;
    const el = document.querySelector(`[data-approval-id="${id}"]`);
    if (el) {
      el.scrollIntoView({ block: "nearest", behavior: "smooth" });
      pendingApprovalScrollRef.current = null;
    }
  }, [messages]);

  useEffect(() => {
    const scrollParent = conversationRef.current;
    if (!scrollParent) return;
    updateJumpToLatestVisibility();
    const handleScroll = () => updateJumpToLatestVisibility();
    scrollParent.addEventListener("scroll", handleScroll, { passive: true });
    return () => scrollParent.removeEventListener("scroll", handleScroll);
  }, [isLoadingHistory, messages.length]);

  useEffect(() => {
    // Capture refs into locals so the cleanup doesn't read a "stale" ref
    // by lint rules. The Record/array is mutated in place across the
    // component's lifetime — `controllers` and `fades` both point at the
    // same live container we just want to drain on unmount.
    const controllers = abortControllersRef.current;
    const fades = fadeTimeoutsRef.current;
    return () => {
      for (const controller of Object.values(controllers)) {
        controller.abort();
      }
      fades.forEach((t) => window.clearTimeout(t));
    };
    // abortControllersRef is a stable ref from useTurnStreamState — listing
    // it keeps exhaustive-deps honest without changing the mount-once run.
  }, [abortControllersRef]);

  // Cache-bust detection. Every response carries X-App-Version set to
  // the server's current build id (see middleware.ts). The client's
  // bundle has that id baked in via NEXT_PUBLIC_BUILD_ID. We probe
  // /api/version on mount, visibilitychange, focus, online, and a
  // 5-minute interval so a long-lived focused tab still notices deploys.
  //
  // We never reload the page automatically. Earlier we did, and it
  // surfaced as "every click jumps me to the top" — once a probe set
  // the flag, the next stream-end finally block would force-reload
  // mid-interaction. Now we just flip `updateAvailable` and the sidebar
  // shows a manual "Update available" button.
  useEffect(() => {
    const clientBuildId = process.env.NEXT_PUBLIC_BUILD_ID ?? "dev";
    if (clientBuildId === "dev") {
      // In dev, the build id changes on every HMR update; skipping
      // the check keeps the tab stable during a `next dev` session.
      return;
    }

    let cancelled = false;

    const probe = async () => {
      if (typeof document === "undefined") return;
      try {
        const res = await fetch("/api/version", { cache: "no-store" });
        if (!res.ok) return;
        const { buildId: serverBuildId } = (await res.json()) as { buildId: string };
        if (cancelled) return;
        if (serverBuildId && serverBuildId !== clientBuildId) {
          setUpdateAvailable(true);
        }
      } catch {
        // Network flake — try again on the next event or interval tick.
      }
    };
    const handle = () => {
      void probe();
    };
    // Fire once on mount in case the user left the tab open across a
    // deploy and we're starting fresh against an already-updated
    // server.
    handle();
    document.addEventListener("visibilitychange", handle);
    window.addEventListener("focus", handle);
    window.addEventListener("online", handle);
    const interval = window.setInterval(handle, 5 * 60 * 1000);
    return () => {
      cancelled = true;
      document.removeEventListener("visibilitychange", handle);
      window.removeEventListener("focus", handle);
      window.removeEventListener("online", handle);
      window.clearInterval(interval);
    };
  }, []);

  // (Initial-load mount effect moved below its callback dependencies — see
  // "mount effects, hoisted below their callback dependencies".)

  // The flat filtered list (folder + labels + title query). When no filter is
  // active this is the full active list, so "select all visible" stays correct.
  const filteredConversations = useMemo(
    () =>
      filterConversations(conversations, {
        folder: filterFolder,
        labels: filterLabels,
        query: sidebarQuery,
      }),
    [conversations, filterFolder, filterLabels, sidebarQuery],
  );

  // With no query, show "default" + "advanced" + the top-ranked list. As
  // soon as the user types, expand the search to the full budget-filtered
  // catalog so they can find cheaper models without leaving the page.
  // Catalog order is price-descending (up to the $30/M ceiling) so the
  // best-quality options show first. Results are capped so the dropdown
  // stays short.
  //
  // Tier slugs deliberately appear twice in the no-query view: once as
  // the friendly alias row at the top (`default` / `advanced`) and once
  // under their lab in the rankings section
  // (`google/gemini-3-flash-preview`, …). Both rows select the same
  // model — users searching by lab or model name shouldn't have to know
  // that "advanced" maps to Claude Sonnet.
  const MAX_SEARCH_RESULTS = 15;
  const filteredRankedModels = useMemo(() => {
    const query = modelSearchQuery.trim().toLowerCase();
    const defaults: RankedModel[] = [
      { slug: DEFAULT_MODEL, name: DEFAULT_MODEL_LABEL },
      { slug: ADVANCED_MODEL, name: ADVANCED_MODEL_LABEL },
    ];

    // Lockdown chats are pinned to the operator-configured allow-list.
    // Build a fixed list that mirrors that allow-list (default first,
    // others in declared order), ignore the freeform search query, and
    // skip the catalog entirely. Server-side enforcement is the source
    // of truth; this filter is just the UX so the user doesn't pick a
    // slug we'd reject anyway.
    if (isLockdown) {
      const allowed = serverConfig.lockdownAllowedModels;
      if (allowed.length === 0) return defaults;
      const seen = new Set<string>();
      const out: RankedModel[] = [];
      for (const slug of allowed) {
        if (seen.has(slug)) continue;
        seen.add(slug);
        const aliased = defaults.find((d) => d.slug === slug);
        if (aliased) {
          out.push(aliased);
          continue;
        }
        const fromRanked = rankedModels.find((m) => m.slug === slug);
        const fromCatalog = catalogModels.find((m) => m.slug === slug);
        const known = fromRanked ?? fromCatalog;
        out.push(known ?? { slug, name: slug });
      }
      return out;
    }

    if (!query) {
      const seen = new Set<string>();
      const out: RankedModel[] = [];
      for (const m of [...defaults, ...rankedModels]) {
        if (seen.has(m.slug)) continue;
        seen.add(m.slug);
        out.push(m);
      }
      return out;
    }
    const source = catalogModels.length > 0 ? catalogModels : rankedModels;
    const matchesQuery = (m: RankedModel) =>
      m.slug.toLowerCase().includes(query) || m.name.toLowerCase().includes(query);
    const seen = new Set<string>();
    const matches: RankedModel[] = [];
    for (const d of defaults) {
      if (matchesQuery(d)) {
        seen.add(d.slug);
        matches.push(d);
      }
    }
    for (const model of source) {
      if (seen.has(model.slug)) continue;
      if (!matchesQuery(model)) continue;
      seen.add(model.slug);
      matches.push(model);
      if (matches.length >= MAX_SEARCH_RESULTS) break;
    }
    return matches;
  }, [rankedModels, catalogModels, modelSearchQuery, isLockdown, serverConfig.lockdownAllowedModels]);

  const loadRankedModels = async () => {
    if (rankedModels.length > 0 || isLoadingRankedModels) return;
    setIsLoadingRankedModels(true);
    try {
      const response = await fetch("/api/model-rankings", { cache: "no-store" });
      if (!response.ok) return;
      const data = await response.json() as { models?: RankedModel[] };
      setRankedModels(data.models ?? []);
    } catch {
      /* optional enhancement only */
    } finally {
      setIsLoadingRankedModels(false);
    }
  };

  // loadCatalogModels pulls the full budget-filtered list of text models
  // from OpenRouter. Larger than the ranked list but still small (~a few
  // hundred entries), so we fetch once per session and search client-side.
  // The response carries `context_length` per model — used by the
  // context-usage indicator (Show details) as well as the picker.
  //
  // Wrapped in useCallback so its identity only changes when the dedup
  // inputs (catalogModels.length / isLoadingCatalog) actually change — the
  // hoisted mount effect lists it as a dependency, so a fresh-every-render
  // identity would re-fire that effect each render. The early-return guard
  // makes any re-fire after the first successful load a no-op.
  const loadCatalogModels = useCallback(async () => {
    if (catalogModels.length > 0 || isLoadingCatalog) return;
    setIsLoadingCatalog(true);
    try {
      const response = await fetch("/api/model-catalog", { cache: "no-store" });
      if (!response.ok) return;
      const data = (await response.json()) as {
        models?: Array<{ slug: string; name: string; context_length?: number; created?: number }>;
      };
      const normalized: RankedModel[] = (data.models ?? []).map((m) => ({
        slug: m.slug,
        name: m.name,
        contextLength: m.context_length,
        created: m.created,
      }));
      setCatalogModels(normalized);
    } catch {
      /* autocomplete enhancement only */
    } finally {
      setIsLoadingCatalog(false);
    }
  }, [catalogModels.length, isLoadingCatalog]);

  // Validate the current model slug against /api/model-check whenever it
  // changes. Debounced because the input calls setSelectedModel on every
  // keystroke. We only block submission when the backend is certain a slug
  // is over budget — unknown/new slugs or network failures keep the
  // previous error cleared so legitimate choices aren't false-positived.
  useEffect(() => {
    const slug = selectedModel.trim();
    if (!slug || slug === DEFAULT_MODEL) {
      // Default / empty slug: drop any stale over-budget error. Deferred
      // to a microtask so the clear lands outside the effect's synchronous
      // phase (no cascading render off the effect body); a guard cancels
      // it if the slug changes again before the microtask runs.
      let cancelled = false;
      queueMicrotask(() => {
        if (!cancelled) setModelError(null);
      });
      return () => {
        cancelled = true;
      };
    }
    const controller = new AbortController();
    const timer = window.setTimeout(async () => {
      try {
        const res = await fetch(`/api/model-check?slug=${encodeURIComponent(slug)}`, {
          cache: "no-store",
          signal: controller.signal,
        });
        if (!res.ok) {
          setModelError(null);
          return;
        }
        const data = (await res.json()) as {
          allowed?: boolean;
          reason?: string;
          message?: string;
          models_url?: string;
        };
        if (data.allowed === false && data.reason === "over_budget" && data.message) {
          setModelError({
            message: data.message,
            modelsUrl: data.models_url ?? "https://openrouter.ai/models",
          });
        } else {
          setModelError(null);
        }
      } catch {
        // Aborted or network error — leave prior state untouched on abort;
        // on true failures fail open so an OpenRouter outage doesn't block
        // the user.
        if (!controller.signal.aborted) setModelError(null);
      }
    }, 300);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [selectedModel]);

  // loadMcpServerCatalog fetches the list of Optional MCP servers for the
  // given conversation, including each server's current opt-in state.
  // The response is used by the Tools picker to render toggles. Safe to
  // call repeatedly — the backend is a cheap JSON read.
  const loadMcpServerCatalog = async (conversationId: string) => {
    if (isLoadingMcpServers) return;
    setIsLoadingMcpServers(true);
    try {
      const response = await fetch(
        `/api/conversations/${encodeURIComponent(conversationId)}/mcp-servers`,
        { cache: "no-store" },
      );
      if (!response.ok) return;
      const data = (await response.json()) as { servers?: MCPServerInfo[] };
      setMcpServers(data.servers ?? []);
    } catch {
      /* non-fatal — picker just stays empty */
    } finally {
      setIsLoadingMcpServers(false);
    }
  };

  // loadMcpServerCatalogPreview fetches the catalog with no per-conversation
  // opt-in state so the Tools picker can render before a conversation row
  // exists (brand-new chat, or zero prior conversations). Called once at
  // startup — per-conversation state takes over once a conversation loads.
  const loadMcpServerCatalogPreview = async () => {
    try {
      const response = await fetch("/api/mcp-servers", { cache: "no-store" });
      if (!response.ok) return;
      const data = (await response.json()) as { servers?: MCPServerInfo[] };
      setMcpServers(data.servers ?? []);
    } catch {
      /* non-fatal */
    }
  };

  // toggleMcpServer optimistically flips the local enabled flag for a
  // server and POSTs the FULL enabled set to the backend. The server is
  // the source of truth for which names are valid optional servers — we
  // just compute the new set from current state and send it. On failure
  // we revert the local state so the UI doesn't claim a change the
  // backend rejected.
  //
  // conversationId === null is the pre-chat case: no row exists yet, so
  // we skip the POST and keep the toggles in local state. They get
  // flushed to the server as part of the first POST /chat body.
  const toggleMcpServer = async (conversationId: string | null, name: string) => {
    const prev = mcpServers;
    const nextServers = prev.map((s) => (s.name === name ? { ...s, enabled: !s.enabled } : s));
    setMcpServers(nextServers);
    if (!conversationId) return;
    const enabledOptional = nextServers.filter((s) => s.enabled).map((s) => s.name);
    try {
      const response = await fetch(
        `/api/conversations/${encodeURIComponent(conversationId)}/mcp-servers`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled_optional: enabledOptional }),
        },
      );
      if (!response.ok) {
        setMcpServers(prev);
      }
    } catch {
      setMcpServers(prev);
    }
  };

  const loadMemories = async () => {
    if (isLoadingMemories) return;
    setIsLoadingMemories(true);
    setMemoryError(null);
    try {
      const response = await fetch("/api/memories", { cache: "no-store" });
      if (!response.ok) throw new Error(await response.text());
      const data = (await response.json()) as { memories?: UserMemory[] };
      setMemories(data.memories ?? []);
    } catch (err) {
      setMemoryError(err instanceof Error && err.message ? err.message : "Unable to load memories.");
    } finally {
      setIsLoadingMemories(false);
    }
  };

  const openMemoryManager = () => {
    setMemoryManagerOpen(true);
    setMemoryView("list");
    setMemoryDraft("");
    setMemoryKindDraft("fact");
    setEditingMemoryId(null);
    void loadMemories();
  };

  const saveMemory = async () => {
    const content = memoryDraft.trim();
    if (!content || isSavingMemory) return;
    setIsSavingMemory(true);
    setMemoryError(null);
    try {
      const url = editingMemoryId
        ? `/api/memories/${encodeURIComponent(editingMemoryId)}`
        : "/api/memories";
      const response = await fetch(url, {
        method: editingMemoryId ? "PATCH" : "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content, kind: memoryKindDraft }),
      });
      if (!response.ok) throw new Error(await response.text());
      setMemoryDraft("");
      setMemoryKindDraft("fact");
      setEditingMemoryId(null);
      await loadMemories();
    } catch (err) {
      setMemoryError(err instanceof Error && err.message ? err.message : "Unable to save memory.");
    } finally {
      setIsSavingMemory(false);
    }
  };

  const deleteMemory = async (id: string) => {
    setMemoryError(null);
    try {
      const response = await fetch(`/api/memories/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!response.ok) throw new Error(await response.text());
      setMemories((prev) => prev.filter((m) => m.id !== id));
      if (editingMemoryId === id) {
        setEditingMemoryId(null);
        setMemoryDraft("");
      }
    } catch (err) {
      setMemoryError(err instanceof Error && err.message ? err.message : "Unable to delete memory.");
    }
  };

  // Partial update for the #515 controls (pin/unpin, retire/restore). The
  // server treats absent fields as untouched, so each action sends only its
  // own flag.
  const patchMemory = async (id: string, patch: { pinned?: boolean; retired?: boolean }) => {
    setMemoryError(null);
    try {
      const response = await fetch(`/api/memories/${encodeURIComponent(id)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      if (!response.ok) throw new Error(await response.text());
      await loadMemories();
    } catch (err) {
      setMemoryError(err instanceof Error && err.message ? err.message : "Unable to update memory.");
    }
  };

  // Keep the browser tab title in sync with the active conversation. The
  // base document title is set by app/layout.tsx; we prepend the active
  // conversation title when there is one so tab switchers show the chat
  // name instead of the bare app name. The base name comes from the runtime
  // client config (branding.app_name) so it's client-agnostic.
  useEffect(() => {
    const base = branding.app_name;
    if (!activeConversationId) {
      document.title = base;
      return;
    }
    const name = activeConversation?.title?.trim();
    document.title = name ? `${name} — ${base}` : base;
  }, [activeConversationId, activeConversation, branding.app_name]);

  // Prefer the server's (possibly auto-summarized) title for the active
  // conversation. Falls back to a first-user-message derivation for brand
  // new / unsaved chats.
  const title = useMemo(() => {
    if (activeConversationId && activeConversation?.title) return activeConversation.title;
    return deriveConversationTitle(messages);
  }, [activeConversationId, activeConversation, messages]);
  // Show ⌘K only to mac users; everyone else gets Ctrl+K. SSR is off
  // for this component (dynamic import in app/page.tsx) so reading
  // navigator at render time is safe — no hydration mismatch.
  const searchShortcut = useMemo(() => {
    if (typeof navigator === "undefined") return "Ctrl+K";
    return /Mac|iPhone|iPad|iPod/i.test(navigator.platform) ? "⌘K" : "Ctrl+K";
  }, []);
  const promptPlaceholder = `Message ${branding.app_name} AI...`;

  // patchAssistantMessage updates a specific message inside a specific
  // conversation's slot. The convId is required because stream events
  // can arrive after the user has navigated to a different chat — we
  // want them to land in the originating conversation, not whatever's
  // currently visible.
  const patchAssistantMessage = (
    convId: string,
    assistantId: number,
    updater: (message: Message) => Message,
  ) => {
    setConvMessages(convId, (current) =>
      current.map((m) => (m.id === assistantId ? updater(m) : m)),
    );
  };

  const refreshConversations = async () => {
    try {
      // Active and archived lists in parallel (#282). The archived fetch is
      // best-effort — a failure leaves the previous archived list in place
      // rather than blanking the section.
      const [activeRes, archivedRes] = await Promise.all([
        fetch("/api/conversations", { cache: "no-store" }),
        fetch("/api/conversations?archived=true", { cache: "no-store" }),
      ]);
      if (activeRes.ok) {
        const data = (await activeRes.json()) as { conversations: ConversationSummary[] | null };
        setConversations(data.conversations ?? []);
      }
      if (archivedRes.ok) {
        const data = (await archivedRes.json()) as { conversations: ConversationSummary[] | null };
        setArchivedConversations(data.conversations ?? []);
      }
    } catch {
      // non-fatal
    }
  };

  const loadConversation = async (
    conversationId: string,
    options: { preserveScroll?: boolean } = {},
  ) => {
    // If this conversation is currently streaming, the local in-memory
    // copy has the in-flight UI updates that the server hasn't
    // persisted yet. Re-fetching would replace those with whatever's
    // in Postgres (which is empty until the stream completes), so we just
    // re-show what we already have.
    if (attachedConvIdsRef.current.has(conversationId)) {
      setActiveConversationId(conversationId);
      const conv = conversations.find((c) => c.id === conversationId);
      if (conv) {
        setSelectedPersona(conv.persona);
        setSelectedModel(conv.model || DEFAULT_MODEL);
      }
      setSidebarOpen(false);
      return;
    }

    setIsLoadingHistory(true);
    try {
      const response = await fetch(`/api/conversations/${conversationId}`, { cache: "no-store" });
      if (!response.ok) throw new Error("Unable to load conversation.");
      const data = (await response.json()) as {
        conversation: ConversationSummary;
        history: HistoryEntry[] | null;
        pending_approvals?: Array<{
          approval_id: string;
          tool: string;
          summary: Approval["summary"];
          expires_at?: number;
        }>;
        pending_memory_proposals?: Array<{
          proposal_id: string;
          content: string;
          kind?: string;
          supersedes_content?: string;
        }>;
      };
      setActiveConversationId(data.conversation.id);
      setSelectedPersona(data.conversation.persona);
      setSelectedModel(data.conversation.model || DEFAULT_MODEL);
      // Reset compaction UI state so the freshly-loaded conversation
      // starts with pre-summary turns collapsed (when present) and
      // any prior error from another chat does not leak into this one.
      setSummaryExpanded(false);
      setSummarizeError(null);
      // Refresh the MCP-server catalog for this conversation so the
      // Tools picker reflects the correct per-conversation opt-in state.
      // Fire-and-forget: the picker shows its own spinner while the
      // fetch is in flight and the conversation body doesn't block on it.
      void loadMcpServerCatalog(data.conversation.id);
      const next = historyToMessages(data.history ?? []);

      // Re-attach any pending approvals + memory proposals onto the last
      // assistant message so a page reload (or the visibilitychange/focus
      // auto-refetch) during an open flow still shows the card. If there's
      // no assistant message yet, create a placeholder one so the card has
      // somewhere to live.
      const pendingApprovals = data.pending_approvals ?? [];
      const pendingMemoryProposals = data.pending_memory_proposals ?? [];
      if (pendingApprovals.length > 0 || pendingMemoryProposals.length > 0) {
        const approvalCards: Approval[] = pendingApprovals.map((p) => ({
          id: p.approval_id,
          tool: p.tool,
          summary: p.summary,
          status: "pending",
          expiresAt: p.expires_at,
        }));
        const memoryCards: MemoryProposal[] = pendingMemoryProposals.map((p) => ({
          id: p.proposal_id,
          content: p.content,
          kind: p.kind,
          supersedesContent: p.supersedes_content,
          status: "pending",
        }));
        const lastAssistantIdx = (() => {
          for (let i = next.length - 1; i >= 0; i--) {
            if (next[i].role === "assistant") return i;
          }
          return -1;
        })();
        if (lastAssistantIdx >= 0) {
          next[lastAssistantIdx] = {
            ...next[lastAssistantIdx],
            approvals: [...(next[lastAssistantIdx].approvals ?? []), ...approvalCards],
            memoryProposals: [
              ...(next[lastAssistantIdx].memoryProposals ?? []),
              ...memoryCards,
            ],
          };
        } else {
          next.push({
            id: Date.now(),
            role: "assistant",
            content: "",
            state: "done",
            approvals: approvalCards,
            memoryProposals: memoryCards,
          });
        }
      }

      // Snap-to-bottom on next render is an "I just opened this chat"
      // affordance. When the caller is a background refresh (tab return
      // via visibilitychange), the user is already in the chat and may
      // have been reading older messages — preserve their scroll
      // position instead of yanking them to the latest reply.
      if (!options.preserveScroll) {
        pendingHistoryScrollRef.current = data.conversation.id;
      }
      setConvMessages(data.conversation.id, next);
      setSidebarOpen(false);
    } finally {
      setIsLoadingHistory(false);
    }

    // After the DB-backed history is rendered, check whether a turn
    // is currently in-flight for this conv and, if so, attach a live
    // SSE stream so the user sees new tokens land as the agent keeps
    // working. Handles the page-refresh-mid-turn scenario: history is
    // empty (server hasn't persisted yet), but /inflight reports
    // inflight:true and /stream replays the complete event sequence.
    void reattachToConv(conversationId);
  };

  const deleteAllUnpinned = async () => {
    const response = await fetch("/api/conversations", { method: "DELETE" });
    if (!response.ok) throw new Error("Unable to delete conversations.");
    // Keep any pinned rows; drop everything else.
    const remaining = conversations.filter((c) => c.pinned);
    setConversations(remaining);
    const active = activeConversationId
      ? remaining.find((c) => c.id === activeConversationId)
      : undefined;
    if (!active) {
      clearConversation();
    }
  };

  // ── Multi-select bulk operations (#279) ─────────────────────────────────
  //
  // Selection is a Set<string> of conversation IDs checked in the sidebar.
  // The bulk action bar appears once ≥1 is selected; Escape clears it and
  // Delete/Backspace opens the confirmation modal (the 3-second countdown is
  // enforced inside the modal component).

  const toggleConversationSelection = (id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const selectAllVisible = () => {
    setSelectedIds((prev) => {
      // Toggling: if every visible conversation is already selected, clear;
      // otherwise select all visible.
      const all = filteredConversations.map((c) => c.id);
      const allSelected = all.length > 0 && all.every((id) => prev.has(id));
      if (allSelected) return new Set();
      return new Set(all);
    });
  };

  const clearSelection = () => setSelectedIds(new Set());

  // bulkDeleteConversations issues a targeted DELETE /conversations with the
  // selected IDs. On success it drops them from local state (and the archived
  // list) and clears the selection. The active conversation, if deleted, is
  // replaced by the first survivor.
  const bulkDeleteConversations = async () => {
    const ids = Array.from(selectedIds);
    if (ids.length === 0) return;
    const response = await fetch("/api/conversations", {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ conversation_ids: ids, confirm: true }),
    });
    if (!response.ok) throw new Error("Unable to delete conversations.");
    const removed = new Set(ids);
    const remaining = conversations.filter((c) => !removed.has(c.id));
    setConversations(remaining);
    setArchivedConversations((current) => current.filter((c) => !removed.has(c.id)));
    setSelectedIds(new Set());
    if (activeConversationId && removed.has(activeConversationId)) {
      const next = remaining[0];
      if (!next) {
        clearConversation();
      } else {
        await loadConversation(next.id);
      }
    }
  };

  // patchConversationIds applies pinned/folder/labels to the given conversation
  // IDs via the live PATCH /conversations/bulk endpoint (#279) and reflects the
  // change locally so the rail updates without a refetch. Undefined fields are
  // dropped by JSON.stringify, so the backend leaves them untouched (COALESCE);
  // folder "" clears the folder, and labels is a full replace.
  const patchConversationIds = async (
    ids: string[],
    changes: { pinned?: boolean; folder?: string; labels?: string[] },
  ) => {
    if (ids.length === 0) return;
    const response = await fetch("/api/conversations/bulk", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ conversation_ids: ids, changes }),
    });
    if (!response.ok) throw new Error("Unable to update conversations.");
    const idSet = new Set(ids);
    setConversations((current) =>
      current.map((c) => {
        if (!idSet.has(c.id)) return c;
        const next = { ...c };
        if (changes.pinned !== undefined) next.pinned = changes.pinned;
        if (changes.folder !== undefined) next.folder = changes.folder;
        if (changes.labels !== undefined) next.labels = changes.labels;
        return next;
      }),
    );
  };

  // bulkPatchConversations targets the current multi-select set.
  const bulkPatchConversations = (changes: { pinned?: boolean; folder?: string; labels?: string[] }) =>
    patchConversationIds(Array.from(selectedIds), changes);

  // setConversationFolder files (folder=name) or unfiles (folder=null) a single
  // conversation from the rail's kebab. Filing auto-pins it — matching the rail's
  // "filing pins it" model; unfiling clears the folder and leaves the pin alone.
  const setConversationFolder = (conversationId: string, folder: string | null) => {
    const changes: { pinned?: boolean; folder?: string } = { folder: folder ?? "" };
    if (folder) changes.pinned = true;
    void patchConversationIds([conversationId], changes);
  };

  // setConversationLabels replaces a single conversation's label set. The bulk
  // endpoint replaces (not appends), so the rail computes the next full set
  // (add/remove) before calling.
  const setConversationLabels = (conversationId: string, labels: string[]) => {
    void patchConversationIds([conversationId], { labels });
  };

  // signOut posts the logout form (preserving the prior <form> POST semantics:
  // the browser navigates to /api/auth/logout, clearing the session cookie).
  const signOut = () => {
    const form = document.createElement("form");
    form.method = "post";
    form.action = "/api/auth/logout";
    document.body.appendChild(form);
    form.submit();
  };

  const deleteConversationById = async (conversationId: string) => {
    const response = await fetch(`/api/conversations/${conversationId}`, { method: "DELETE" });
    if (!response.ok) throw new Error("Unable to delete conversation.");
    const remaining = conversations.filter((c) => c.id !== conversationId);
    setConversations(remaining);
    // Also drop it from the archived list (#282): delete is reachable from the
    // Archived section, and the two lists are disjoint, so filtering both is
    // safe — it's a no-op on whichever list didn't hold the row.
    setArchivedConversations((current) => current.filter((c) => c.id !== conversationId));
    clearConvSlot(conversationId);
    if (activeConversationId !== conversationId) return;
    const nextConversation = remaining[0];
    if (!nextConversation) {
      clearConversation();
      return;
    }
    await loadConversation(nextConversation.id);
  };

  const confirmDeleteConversation = async () => {
    if (!pendingDeleteConversation) return;
    const id = pendingDeleteConversation.id;
    setPendingDeleteConversation(null);
    await deleteConversationById(id);
  };

  // summaryIndex is the position of the (at most one) summary
  // message in the active conversation. Recomputed on every messages
  // change. -1 when no summary exists. Drives the collapse-pre-summary
  // expander and the SummaryBanner positioning in the render loop.
  const summaryIndex = useMemo(() => {
    for (let i = 0; i < messages.length; i++) {
      if (messages[i].kind === "summary") return i;
    }
    return -1;
  }, [messages]);

  // Precompute last message IDs to avoid an O(N²) bottleneck in the render loop
  // where we previously searched backwards for *each* message.
  const lastUserMessageId = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].role === "user") return messages[i].id;
    }
    return null;
  }, [messages]);

  const lastAssistantMessageId = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].role === "assistant") return messages[i].id;
    }
    return null;
  }, [messages]);

  // summarizeConversation triggers the user-initiated "summarize and
  // continue" flow. POSTs to the new endpoint, optimistic-disables
  // the button, and on success reloads the conversation so the new
  // summary message is in messages with everything around it
  // collapsed by default. Replace semantics on the backend mean
  // calling this twice produces a single replacement, never a chain.
  const summarizeConversation = async () => {
    if (!activeConversationId) return;
    if (isStreaming || isSummarizing) return;
    setIsSummarizing(true);
    setSummarizeError(null);
    setSummarizeStream("");
    setSummarizeStartedAt(nowMs());
    try {
      const response = await fetch(
        `/api/conversations/${encodeURIComponent(activeConversationId)}/summarize`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ model: selectedModel }),
        },
      );
      if (!response.ok) {
        if (response.status === 409) {
          throw new Error("A turn is currently running — wait for it to finish before compacting.");
        }
        const text = await response.text();
        throw new Error(text || `Compact failed (HTTP ${response.status}).`);
      }
      if (!response.body) {
        throw new Error("Compact stream missing body.");
      }
      // Drain the SSE stream. summary.delta events feed the progress
      // card; summary.completed signals success and we reload the
      // canonical history; summary.error surfaces a mid-stream model
      // failure (rare — pre-stream errors come back as HTTP codes).
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      let streamErr: string | null = null;
      let completed = false;
      while (!completed) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parsed = parseSseChunk(buffer);
        buffer = parsed.remainder;
        for (const event of parsed.events) {
          if (event.event === "summary.delta") {
            try {
              const p = JSON.parse(event.data) as { text?: string };
              if (p.text) setSummarizeStream((prev) => prev + p.text);
            } catch {
              /* malformed delta — drop */
            }
          } else if (event.event === "summary.completed") {
            completed = true;
          } else if (event.event === "summary.error") {
            try {
              const p = JSON.parse(event.data) as { message?: string };
              streamErr = p.message ?? "Compact failed.";
            } catch {
              streamErr = "Compact failed.";
            }
            completed = true;
          }
        }
      }
      if (streamErr) throw new Error(streamErr);
      // Reload from the source of truth so the summary message and
      // any side-effects (updated_at bump, etc.) land cleanly. The
      // collapsed-range UI defaults to closed on each load, so the
      // user sees "+ N earlier turns" right after the summarize call
      // returns.
      await loadConversation(activeConversationId);
    } catch (err) {
      setSummarizeError(err instanceof Error ? err.message : "Compact failed.");
    } finally {
      setIsSummarizing(false);
      setSummarizeStream("");
      setSummarizeStartedAt(null);
    }
  };

  const togglePin = async (conversation: ConversationSummary) => {
    const nextPinned = !conversation.pinned;
    // Optimistic update
    setConversations((current) =>
      current
        .map((c) => (c.id === conversation.id ? { ...c, pinned: nextPinned } : c))
        .sort((a, b) => {
          if (a.pinned !== b.pinned) return a.pinned ? -1 : 1;
          return b.updated_at - a.updated_at;
        }),
    );
    const response = await fetch(`/api/conversations/${conversation.id}/pin`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ pinned: nextPinned }),
    });
    if (!response.ok) {
      // revert on failure
      await refreshConversations();
    }
  };

  // branchFromMessage forks the active conversation at a chosen PERSISTED message
  // into a new independent thread (#454), then opens it. The Branch button only
  // renders for messages carrying a dbId (persisted), so message.dbId is present
  // here; we still guard defensively. On success we refresh the sidebar (so the
  // new branch appears) and load it; failures mirror the other conversation
  // actions (log + no-op — the user simply stays put).
  const branchFromMessage = async (message: Message) => {
    const parentId = activeConversationId;
    if (!parentId || !message.dbId) return;
    try {
      const response = await fetch(`/api/conversations/${parentId}/branch`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ branch_point_message_id: message.dbId }),
      });
      if (!response.ok) {
        console.error("branch failed:", response.status, await response.text());
        return;
      }
      const branch = (await response.json()) as { id?: string };
      if (!branch.id) return;
      await refreshConversations();
      await loadConversation(branch.id);
    } catch (err) {
      console.error("branch error:", err);
    }
  };

  // startProjectChat creates a conversation bound to a project (#509) — the
  // server validates membership and inherits the project's defaults +
  // curated connector selection — then opens it.
  const startProjectChat = async (projectID: string) => {
    try {
      const response = await fetch("/api/conversations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title: "New conversation", project_id: projectID }),
      });
      if (!response.ok) {
        console.error("project chat failed:", response.status, await response.text());
        return;
      }
      const conv = (await response.json()) as { id?: string };
      if (!conv.id) return;
      setProjectsOpen(false);
      await refreshConversations();
      await loadConversation(conv.id);
    } catch (err) {
      console.error("project chat error:", err);
    }
  };

  // buildShareUrl turns a share token into the absolute, copy-paste link a
  // recipient opens. window.origin is the deployment origin the user is on (#226).
  const buildShareUrl = (token: string) =>
    typeof window !== "undefined" ? `${window.location.origin}/shared/${token}` : `/shared/${token}`;

  // patchShareToken updates a conversation's share_token in BOTH the active and
  // archived lists so the 🔗 badge + Share/Unshare menu reflect reality whichever
  // section the row lives in.
  const patchShareToken = (id: string, token: string) => {
    const patch = (c: ConversationSummary) => (c.id === id ? { ...c, share_token: token } : c);
    setConversations((current) => current.map(patch));
    setArchivedConversations((current) => current.map(patch));
  };

  // shareConversation issues a public read-only link and copies it to the
  // clipboard (#226). Optimistic; reverts via refetch on backend failure.
  // Returns true only when the link was both created AND copied, so the sidebar
  // shows "Copied!" honestly (a blocked clipboard returns false → no flash, but
  // the 🔗 badge still appears as the share succeeded).
  const shareConversation = async (conversation: ConversationSummary): Promise<boolean> => {
    const response = await fetch(`/api/conversations/${conversation.id}/share`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    });
    if (!response.ok) {
      await refreshConversations();
      return false;
    }
    const data = (await response.json()) as { token?: string };
    const token = data.token ?? "";
    patchShareToken(conversation.id, token);
    if (!token) return false;
    try {
      await navigator.clipboard.writeText(buildShareUrl(token));
      return true;
    } catch {
      // Clipboard may be blocked (no user gesture / insecure context); the link
      // is still reachable via the Copy-link action once the badge appears.
      return false;
    }
  };

  // unshareConversation revokes the public link (#226).
  const unshareConversation = async (conversation: ConversationSummary) => {
    patchShareToken(conversation.id, "");
    const response = await fetch(`/api/conversations/${conversation.id}/share`, { method: "DELETE" });
    if (!response.ok) {
      await refreshConversations();
    }
  };

  // copyShareLink re-copies an already-shared conversation's link (#226).
  const copyShareLink = async (conversation: ConversationSummary): Promise<boolean> => {
    if (!conversation.share_token) return false;
    try {
      await navigator.clipboard.writeText(buildShareUrl(conversation.share_token));
      return true;
    } catch {
      return false;
    }
  };

  // toggleArchive moves a conversation between the active and archived lists
  // (#282). Optimistic: the row hops sections immediately; on a backend error
  // we re-fetch to restore the truth. Archiving also clears the pin (the
  // backend enforces this), so the optimistic copy drops it too.
  const toggleArchive = async (conversation: ConversationSummary, archived: boolean) => {
    if (archived) {
      setConversations((current) => current.filter((c) => c.id !== conversation.id));
      setArchivedConversations((current) =>
        [{ ...conversation, pinned: false, archived_at: Math.floor(Date.now() / 1000) }, ...current].sort(
          (a, b) => b.updated_at - a.updated_at,
        ),
      );
    } else {
      setArchivedConversations((current) => current.filter((c) => c.id !== conversation.id));
      setConversations((current) =>
        [{ ...conversation, archived_at: null }, ...current].sort((a, b) => {
          if (a.pinned !== b.pinned) return a.pinned ? -1 : 1;
          return b.updated_at - a.updated_at;
        }),
      );
    }
    const response = await fetch(`/api/conversations/${conversation.id}/archive`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ archived }),
    });
    if (!response.ok) {
      await refreshConversations();
    }
  };

  const renameConversation = async (
    conversationId: string,
    nextTitle: string,
  ): Promise<boolean> => {
    const trimmed = nextTitle.trim();
    if (!trimmed) return false;
    const before = conversations.find((c) => c.id === conversationId);
    if (!before) return false;
    if (before.title === trimmed) return true;
    setConversations((current) =>
      current.map((c) => (c.id === conversationId ? { ...c, title: trimmed } : c)),
    );
    const response = await fetch(`/api/conversations/${conversationId}/rename`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ title: trimmed }),
    });
    if (!response.ok) {
      await refreshConversations();
      return false;
    }
    return true;
  };

  const downloadConversation = async (conversation: ConversationSummary) => {
    try {
      const response = await fetch(`/api/conversations/${conversation.id}/export`, {
        method: "GET",
      });
      if (!response.ok) {
        console.error("export failed", response.status, await response.text());
        return;
      }
      // Prefer the filename chosen by the server (Content-Disposition),
      // fall back to a client-side slug if that header is stripped by a
      // proxy somewhere.
      const cd = response.headers.get("Content-Disposition") ?? "";
      const match = /filename="([^"]+)"/i.exec(cd);
      const filename = match?.[1] ?? `${conversation.id}.json`;
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = filename;
      document.body.appendChild(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      console.error("export failed", err);
    }
  };

  // promoteConversation asks the server to synthesize a recurring-task proposal
  // from this conversation and stage it as a schedule_task approval (#455). On
  // success it opens the conversation and re-hydrates, so the pre-filled
  // ScheduleTaskCard appears for the user to review and approve (or cancel) —
  // the task is created only on approve, via the existing #239 path.
  const promoteConversation = async (conversation: ConversationSummary) => {
    try {
      const response = await fetch(`/api/conversations/${conversation.id}/promote-to-task`, {
        method: "POST",
      });
      if (!response.ok) {
        console.error("promote-to-task failed", response.status, await response.text());
        return;
      }
      await loadConversation(conversation.id);
    } catch (err) {
      console.error("promote-to-task failed", err);
    }
  };

  const startThinkingCrossfade = (assistantId: number) => {
    setCrossfadingMessageIds((current) =>
      current.includes(assistantId) ? current : [...current, assistantId],
    );
    const timeoutId = window.setTimeout(() => {
      setCrossfadingMessageIds((current) => current.filter((id) => id !== assistantId));
      fadeTimeoutsRef.current = fadeTimeoutsRef.current.filter((v) => v !== timeoutId);
    }, 220);
    fadeTimeoutsRef.current.push(timeoutId);
  };

  const clearConversation = (opts?: { lockdown?: boolean }) => {
    // The slot the user is staring at. Only tear it down when it's
    // idle — if a turn is in flight (including the brief window before
    // a per-submission pending key is promoted to a real conv id),
    // leave the slot intact so the stream finishes in the background.
    // The sidebar dot keeps marking the conv as working; the user can
    // navigate back later to see the result.
    const key = activeConversationIdRef.current;
    if (key !== null && !streamingConvsRef.current.has(key)) {
      abortControllersRef.current[key]?.abort();
      delete abortControllersRef.current[key];
      markConvIdle(key);
      clearConvSlot(key);
    }
    // Composer state lives on the PENDING singleton for the empty
    // new-chat view; resetting it gives the user a clean slate for
    // the chat they're about to compose.
    setPromptForKey(PENDING_CONV_KEY, "");
    setPendingAttachmentsForKey(PENDING_CONV_KEY, []);
    setAttachmentErrorForKey(PENDING_CONV_KEY, null);
    fadeTimeoutsRef.current.forEach((t) => window.clearTimeout(t));
    fadeTimeoutsRef.current = [];
    setCrossfadingMessageIds([]);
    activeConversationIdRef.current = null;
    setActiveConversationId(null);
    setActivePillId(null);
    setSidebarOpen(false);
    // New chat = fresh opt-in state. Keep the catalog but reset each toggle
    // to its default (default-on servers like gamma come back on; everything
    // else clears) so the Tools picker doesn't inherit the previous
    // conversation's selection.
    setMcpServers((prev) =>
      prev.map((s) => ({ ...s, enabled: s.enabled_by_default ?? false })),
    );
    // Lockdown is set per-conversation. New regular chat clears it;
    // new lockdown chat sets it. In LockdownOnly server mode every
    // chat is implicitly lockdown — clicking the regular "Clear"
    // button or the (hidden) plain + still produces a lockdown chat.
    // The backend force-flags it server-side either way, but mirroring
    // it here keeps the UI honest (badge stays on, model picker stays
    // filtered).
    const explicit = opts?.lockdown === true;
    const lockdown =
      serverConfig.lockdownAvailable && (explicit || serverConfig.lockdownOnly);
    setPendingLockdown(lockdown);
    // Lockdown chats are pinned to the allow-list. Default both modes
    // to DEFAULT_MODEL — for lockdown that's also the first allowed
    // slug, and for normal chat it's the product default.
    setSelectedModel(DEFAULT_MODEL);
    promptRef.current?.focus();
  };

  // ── shortcut catalog ─────────────────────────────────────────────────
  //
  // One declarative list, registered through the shared useKeyboardShortcuts
  // hook (#306). Each global action here has an equivalent mouse affordance
  // elsewhere in the shell (the sidebar "New chat" button, the search button,
  // the composer) — no keyboard-only paths. The "?" overlay below is generated
  // from `shortcutHelpGroups`, keeping the documented keys in step with the
  // wired ones.
  //
  // Platform: shortcuts marked `mod: true` fire on ⌘ (macOS) / Ctrl (elsewhere).
  // Typing safety: bare-letter shortcuts (`?`) are suppressed while a text field
  // is focused so they don't eat keystrokes; ⌘/Ctrl combos opt in via
  // allowInInput so they still work from inside the composer. The composer's own
  // Enter / Shift+Enter handling lives on its textarea and is untouched.
  const closeOverlays = useCallback(() => {
    // Escape collapses whatever transient surface is open. The search and
    // shortcut overlays also carry their own focus-independent Escape listeners
    // (they're mounted-while-open), so this is the catch-all for the sidebar.
    setShortcutsOpen(false);
    setSearchOpen(false);
    setSidebarOpen(false);
  }, []);

  // Multi-select keyboard support (#279): Escape clears the selection; Delete
  // / Backspace (when not typing in an input) opens the bulk-delete confirm
  // modal. The 3-second countdown is enforced inside the modal. Skipped when a
  // transient overlay (search / shortcuts / pending single-delete) is open so
  // the keystroke is handled by that surface instead.
  useEffect(() => {
    if (selectedIds.size === 0) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        setSelectedIds(new Set());
        return;
      }
      if (e.key === "Delete" || e.key === "Backspace") {
        const target = e.target as HTMLElement | null;
        const tag = target?.tagName;
        if (tag === "INPUT" || tag === "TEXTAREA" || target?.isContentEditable) return;
        if (searchOpen || shortcutsOpen || pendingDeleteConversation) return;
        e.preventDefault();
        setBulkDeleteConfirm(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectedIds.size, searchOpen, shortcutsOpen, pendingDeleteConversation]);

  const shortcuts = useMemo<KeyboardShortcut[]>(
    () => [
      {
        key: "k",
        mod: true,
        allowInInput: true,
        handler: () => setSearchOpen(true),
      },
      {
        // Cmd/Ctrl+F is an alternate binding for search, but NOT while typing —
        // there it must fall through to the browser's in-page find.
        key: "f",
        mod: true,
        allowInInput: false,
        handler: () => setSearchOpen(true),
      },
      {
        key: "n",
        mod: true,
        shift: false,
        allowInInput: true,
        handler: () => clearConversation(),
      },
      {
        // Focus the composer from anywhere (mirrors clicking into it).
        key: "j",
        mod: true,
        allowInInput: true,
        handler: () => promptRef.current?.focus(),
      },
      {
        // "?" opens the help overlay. Bare key, so it's suppressed while typing.
        key: "?",
        handler: () => setShortcutsOpen(true),
      },
      {
        key: "Escape",
        allowInInput: true,
        handler: closeOverlays,
      },
    ],
    // clearConversation is a fresh closure each render but stable in behavior;
    // the hook reads the list through a ref so this list is rebuilt cheaply and
    // never re-subscribes the listener.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [closeOverlays],
  );
  useKeyboardShortcuts(shortcuts);

  // ── the streaming turn/SSE loop ───────────────────────────────────────
  //
  // The loop itself — applyStreamEvent / pumpStreamResponse / reattachToConv
  // / streamTurn / submitPrompt / retry / regenerate / resend — lives in
  // useTurnStream (issue #401 step 3 / #435). This is a behavior-preserving
  // relocation: the bodies are unchanged and read everything through this one
  // `deps` object. useTurnStreamState (transport state) and
  // usePerConvComposerState (composer state) stay instantiated above because
  // their values are also read during render and by component-level effects;
  // their members flow into the loop as deps. The `satisfies TurnStreamDeps`
  // annotation makes tsc flag any missing or mistyped dep at THIS assembly
  // site, not just inside a moved body.
  const turnStreamDeps = {
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
  } satisfies TurnStreamDeps;
  const {
    reattachToConv,
    submitPrompt,
    regenerateLastAssistant,
    resendUserMessage,
    retryLastUserMessage,
  } = useTurnStream(turnStreamDeps);

  // Latest-callback refs for the two mount-once effects below. reattachToConv
  // and loadConversation are mutually recursive, so neither can be expressed
  // as a clean declared-before-use useCallback; refreshConversations and
  // loadMcpServerCatalogPreview are leaves but are recreated every render.
  // Rather than thread render-recreated identities into the effects' dep
  // arrays (which would re-subscribe/re-run them every render) or stale the
  // closures with empty deps, we keep their latest identities in refs and
  // call through the refs from inside the effects. The effects then depend
  // only on these stable refs, so their dep arrays are honest with no
  // suppression. Reading a ref *inside an effect or event handler* (never
  // during render) is the supported pattern.
  const reattachToConvRef = useRef(reattachToConv);
  const loadConversationRef = useRef(loadConversation);
  const refreshConversationsRef = useRef(refreshConversations);
  const loadMcpServerCatalogPreviewRef = useRef(loadMcpServerCatalogPreview);
  useEffect(() => {
    reattachToConvRef.current = reattachToConv;
    loadConversationRef.current = loadConversation;
    refreshConversationsRef.current = refreshConversations;
    loadMcpServerCatalogPreviewRef.current = loadMcpServerCatalogPreview;
  });

  // ── mount effects, hoisted below their callback dependencies ────────────
  // These three effects all kick off work via callbacks declared above
  // (loadCatalogModels, reattachToConv, refreshConversations,
  // loadConversation, loadMcpServerCatalogPreview). They originally sat near
  // the other mount effects at the top of the component, but reading a
  // callback before its declaration trips react-hooks/immutability ("Cannot
  // access variable before it is declared"). Placing them here — after every
  // callback they reference — keeps the lint gate honest without weakening
  // behavior. Their relative order (catalog → visibility-refresh →
  // initial-load) is unchanged from before.

  // Load the catalog once so the context-usage ring (next to the
  // composer) and the stats-panel chip can both resolve the selected
  // model's context window. Cheap — one fetch per session, server-side
  // cached for 24h — and a no-op when already loaded. Fires regardless
  // of showStats since the composer ring is always visible.
  //
  // The kickoff is deferred to a microtask: loadCatalogModels flips
  // setIsLoadingCatalog(true) synchronously, and calling it directly in
  // the effect body would run that setState in the effect's synchronous
  // phase (react-hooks/set-state-in-effect). Deferring moves the first
  // setState out of that phase; a guard skips the call if we unmount
  // before the microtask runs.
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void loadCatalogModels();
    });
    return () => {
      cancelled = true;
    };
  }, [loadCatalogModels]);

  // Refresh the active conversation when the tab/window becomes visible
  // again. The server now keeps generating after the SSE connection
  // drops, so a turn the user kicked off before locking their phone (or
  // backgrounding the tab) often completes server-side while they're
  // away. Without this, returning to a stale tab would still show the
  // truncated reply that was on screen at drop time.
  //
  // Skipped while a stream is in flight — loadConversation short-
  // circuits in that case anyway, and we don't want to hit the server
  // for a refetch we wouldn't apply.
  useEffect(() => {
    const refreshIfStale = async () => {
      if (typeof document === "undefined") return;
      if (document.visibilityState !== "visible") return;
      const convId = activeConversationIdRef.current;
      if (!convId) return;
      if (attachedConvIdsRef.current.has(convId)) return;

      // First try to reattach to any in-flight turn so the user sees
      // live tokens resume. If nothing's in-flight, fall back to a
      // plain DB reload in case a turn completed while we were away.
      // Callbacks are read through latest-refs (see the ref bundle above)
      // so this mount-once listener never goes stale yet keeps `[]` deps.
      await reattachToConvRef.current(convId);
      if (attachedConvIdsRef.current.has(convId)) return;

      // Refresh the sidebar list unconditionally — small payload, no
      // chat repaint, and titles/updated_at may have moved if turns
      // completed elsewhere.
      void refreshConversationsRef.current();

      // Only refetch the active conversation when our local copy looks
      // mid-turn — i.e. some message is still in `streaming` / `thinking`.
      // That's the signal a turn was in flight when the user backgrounded
      // the tab; the SSE buffer has since been evicted (else reattach
      // would have caught it above) and Postgres has the canonical reply
      // we need to swap in. For a clean idle tab, skip: refetching
      // repaints the entire conversation, costs a roundtrip, and breaks
      // transient UI state (open dropdowns, in-progress edits) every
      // single time the user flips tabs — which made the chat feel like
      // it was reloading on every return.
      const localMsgs = messagesByConvRef.current[convId];
      const hasStaleStream = localMsgs?.some(
        (m) => m.state === "streaming" || m.state === "thinking",
      );
      if (!hasStaleStream) return;

      // preserveScroll: the user was already on this conversation and may
      // have been mid-read. Even when we do refetch (turn dropped while
      // away), snap-to-bottom on tab return is jarring — keep their
      // scroll position and let the live "follow along" auto-scroll
      // handle anything they were already at the bottom of.
      void loadConversationRef.current(convId, { preserveScroll: true });
    };
    const handle = () => {
      void refreshIfStale();
    };
    document.addEventListener("visibilitychange", handle);
    // `focus` covers desktop window-focus changes inside the same
    // visible viewport; `online` covers network-restore events that
    // neither fire. Mobile lock/unlock goes through visibilitychange.
    window.addEventListener("focus", handle);
    window.addEventListener("online", handle);
    return () => {
      document.removeEventListener("visibilitychange", handle);
      window.removeEventListener("focus", handle);
      window.removeEventListener("online", handle);
    };
    // Mount-once: the listeners are registered/torn down a single time and
    // call reattachToConv / refreshConversations / loadConversation through
    // their latest-refs, so there are no reactive dependencies to track.
    // attachedConvIdsRef is a stable ref from useTurnStreamState — listing
    // it keeps exhaustive-deps honest without changing the mount-once run.
  }, [attachedConvIdsRef]);

  // Initial load: session, personas, conversations, most-recent conversation.
  useEffect(() => {
    let cancelled = false;

    const loadInitialState = async () => {
      try {
        const sessionResponse = await fetch("/api/session", { cache: "no-store" });
        if (!sessionResponse.ok) {
          window.location.href = "/login";
          return;
        }
        const sessionData = (await sessionResponse.json()) as { email: string };
        if (cancelled) return;
        setUserEmail(sessionData.email);

        // Personas
        try {
          const pr = await fetch("/api/personas", { cache: "no-store" });
          if (pr.ok) {
            const pd = (await pr.json()) as PersonasResponse;
            if (!cancelled) {
              setPersonas(pd.personas);
              setSelectedPersona(pd.default);
            }
          }
        } catch {
          // Personas are nice-to-have; the server falls back to default.
        }

        // Bundle skill roster for the composer "/" autocomplete (#513).
        // Best-effort: an older server without /skills just leaves the
        // autocomplete empty.
        try {
          const sr = await fetch("/api/skills", { cache: "no-store" });
          if (sr.ok) {
            const sd = (await sr.json()) as { skills?: SkillInfo[] };
            if (!cancelled) setSkills(sd.skills ?? []);
          }
        } catch {
          // Optional nicety — plain "/text" messages still send fine.
        }

        // Prime the Tools picker catalog so it renders for new chats too.
        // loadConversation will overwrite with per-conversation enabled
        // state once an existing conversation is opened. Called through a
        // latest-ref so this mount-once bootstrap keeps `[]` deps.
        if (!cancelled) void loadMcpServerCatalogPreviewRef.current();

        // Capability fetch — currently just lockdown availability.
        // Best-effort: a 404 / network error means the older server
        // doesn't expose this endpoint, so we keep the feature off.
        try {
          const cfgRes = await fetch("/api/server-config", { cache: "no-store" });
          if (cfgRes.ok) {
            const cfg = (await cfgRes.json()) as {
              lockdown_available: boolean;
              lockdown_only: boolean;
              lockdown_allowed_models: string[] | null;
            };
            if (!cancelled) {
              setServerConfig({
                lockdownAvailable: cfg.lockdown_available === true,
                lockdownOnly: cfg.lockdown_only === true,
                lockdownAllowedModels: cfg.lockdown_allowed_models ?? [],
              });
            }
          }
        } catch {
          // Optional capability — leave lockdown off when the server
          // is too old to advertise it.
        }

        const conversationsResponse = await fetch("/api/conversations", { cache: "no-store" });
        if (!conversationsResponse.ok) {
          window.location.href = "/login";
          return;
        }
        const conversationsData = (await conversationsResponse.json()) as {
          conversations: ConversationSummary[] | null;
        };
        if (cancelled) return;
        const convs = conversationsData.conversations ?? [];
        setConversations(convs);

        const latest = convs[0];
        if (!latest) {
          setActiveConversationId(null);
          return;
        }
        await loadConversationRef.current(latest.id);
      } finally {
        if (!cancelled) setIsLoadingHistory(false);
      }
    };

    void loadInitialState();
    return () => {
      cancelled = true;
    };
    // Mount-once bootstrap: re-running it would re-fetch the session and
    // clobber the active conversation. It calls loadMcpServerCatalogPreview
    // and loadConversation through their latest-refs (see the ref bundle
    // above), so there are no reactive dependencies to declare.
  }, []);

  const toggleShowStats = () => {
    setShowStats((prev) => {
      const next = !prev;
      window.localStorage.setItem("chat-show-stats", next ? "1" : "0");
      return next;
    });
  };

  const jumpToLatest = () => {
    streamEndRef.current?.scrollIntoView({ block: "end", behavior: "smooth" });
  };

  const addAttachmentFiles = (files: FileList | null) => {
    if (!files || files.length === 0) return;
    setAttachmentError(null);
    const additions: PendingAttachment[] = [];
    for (const file of Array.from(files)) {
      additions.push({
        clientId: crypto.randomUUID(),
        file,
        status: "pending",
        name: file.name,
        size: file.size,
        mime: file.type || "",
      });
    }
    setPendingAttachments((prev) => [...prev, ...additions]);
  };

  const removePendingAttachment = (clientId: string) => {
    setPendingAttachments((prev) => {
      const next = prev.filter((a) => a.clientId !== clientId);
      // Re-arm the spreadsheet nudge once the composer empties so the
      // next heavy upload can surface the banner again. Previously a
      // synchronous effect watched pendingAttachments.length for this;
      // doing it in the handler keeps the reset off the render path.
      if (next.length === 0) setSpreadsheetNudgeDismissed(false);
      return next;
    });
    setAttachmentError(null);
  };

  return (
    <div
      className={`h-[100dvh] overflow-hidden bg-[var(--gradient-bg-home-signature)] text-[var(--color-text-primary)] ${sidebarOpen ? "lg:overflow-hidden" : ""}`}
    >
      {/* grid-cols-[minmax(0,1fr)] on mobile: without it the single implicit
          column auto-sizes to max-content of the main column, which can
          exceed the viewport when main's own overflow:hidden lets its grid
          track grow. Explicit minmax(0, 1fr) clamps it. lg: swaps in the
          two-column sidebar+main layout for desktop. */}
      <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] lg:grid-cols-[18rem_minmax(0,1fr)]">
        <ConversationSidebar
          sidebarOpen={sidebarOpen}
          setSidebarOpen={setSidebarOpen}
          branding={branding}
          serverConfig={serverConfig}
          userEmail={userEmail}
          onSignOut={signOut}
          clearConversation={clearConversation}
          sidebarQuery={sidebarQuery}
          setSidebarQuery={setSidebarQuery}
          searchRef={searchRef}
          filterFolder={filterFolder}
          setFilterFolder={setFilterFolder}
          filterLabels={filterLabels}
          setFilterLabels={setFilterLabels}
          isLoadingHistory={isLoadingHistory}
          conversations={conversations}
          filteredConversations={filteredConversations}
          activeConversationId={activeConversationId}
          loadConversation={loadConversation}
          streamingConvs={streamingConvs}
          togglePin={togglePin}
          toggleArchive={toggleArchive}
          renameConversation={renameConversation}
          downloadConversation={downloadConversation}
          promoteConversation={promoteConversation}
          setPendingDeleteConversation={setPendingDeleteConversation}
          setConversationFolder={setConversationFolder}
          setConversationLabels={setConversationLabels}
          shareConversation={shareConversation}
          unshareConversation={unshareConversation}
          copyShareLink={copyShareLink}
          archivedConversations={archivedConversations}
          showArchived={showArchived}
          setShowArchived={setShowArchived}
          updateAvailable={updateAvailable}
          setConfirmBulkDelete={setConfirmBulkDelete}
          selectedIds={selectedIds}
          onToggleSelection={toggleConversationSelection}
          onSelectAllVisible={selectAllVisible}
          onClearSelection={clearSelection}
          onBulkDelete={() => setBulkDeleteConfirm(true)}
          onBulkPin={() => void bulkPatchConversations({ pinned: true })}
          onBulkUnpin={() => void bulkPatchConversations({ pinned: false })}
          onBulkMoveFolder={(folder) => {
            if (folder === "") return;
            void bulkPatchConversations({ folder, pinned: true });
          }}
          onBulkAddLabel={(label) => {
            if (label === "") return;
            void bulkPatchConversations({ labels: [label] });
          }}
        />

        {searchOpen ? (
          <SearchBar
            onClose={() => setSearchOpen(false)}
            onSelect={(conversationId) => {
              setSearchOpen(false);
              void loadConversation(conversationId);
            }}
          />
        ) : null}

        {shortcutsOpen ? (
          <KeyboardShortcutsOverlay
            groups={shortcutHelpGroups}
            onClose={() => setShortcutsOpen(false)}
          />
        ) : null}

        {confirmBulkDelete ? (
          <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
            <button
              aria-label="Close delete-all confirmation"
              className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
              type="button"
              onClick={() => setConfirmBulkDelete(false)}
            />
            <div className="relative z-10 w-full max-w-[26rem] rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
              <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
                Delete all unpinned chats?
              </h2>
              <p className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
                {conversations.filter((c) => !c.pinned).length} conversation
                {conversations.filter((c) => !c.pinned).length === 1 ? "" : "s"} will be
                removed. Pinned chats are kept. This cannot be undone.
              </p>
              <div className="flex items-center justify-end gap-2">
                <button
                  type="button"
                  className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                  onClick={() => setConfirmBulkDelete(false)}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  className="rounded-full bg-[var(--color-danger)] px-4 py-2 text-[0.8125rem] font-medium text-white transition hover:opacity-90"
                  onClick={async () => {
                    setConfirmBulkDelete(false);
                    await deleteAllUnpinned();
                  }}
                >
                  Delete all
                </button>
              </div>
            </div>
          </div>
        ) : null}

        {/* Multi-select bulk delete confirmation (#279). Shows the exact
            selection count and disables the confirm button for 3 seconds (with
            a visible countdown) to discourage impulsive bulk wipes. Mounted
            only while open so the countdown resets on each invocation. */}
        {bulkDeleteConfirm ? (
          <BulkDeleteConfirmModal
            count={selectedIds.size}
            onCancel={() => setBulkDeleteConfirm(false)}
            onConfirm={async () => {
              setBulkDeleteConfirm(false);
              await bulkDeleteConversations();
            }}
          />
        ) : null}

        {projectsOpen ? (
          <ProjectsModal
            userEmail={userEmail}
            onClose={() => setProjectsOpen(false)}
            onStartChat={(id) => void startProjectChat(id)}
          />
        ) : null}

        {memoryManagerOpen ? (
          <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
            <button
              aria-label="Close memories"
              className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
              type="button"
              onClick={() => setMemoryManagerOpen(false)}
            />
            <div className="relative z-10 flex max-h-[88vh] w-full max-w-[34rem] flex-col gap-4 overflow-hidden rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">Memories</h2>
                  <p className="mt-1 text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
                    Saved memories are scoped to {userEmail || "this user"} and are added to future chats.
                  </p>
                </div>
                <button
                  type="button"
                  aria-label="Close memories"
                  className="inline-flex size-9 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                  onClick={() => setMemoryManagerOpen(false)}
                >
                  <Icon name="close" className="size-4" />
                </button>
              </div>

              {/* Knowledge-graph tab (#523): a compact derived view over the
                  same records; the record list stays the default. */}
              <div className="flex items-center gap-1">
                {(["list", "graph"] as const).map((view) => (
                  <button
                    key={view}
                    type="button"
                    className={`rounded-full border px-3 py-1 text-[0.75rem] transition ${
                      memoryView === view
                        ? "border-[var(--color-text-primary)] text-[var(--color-text-primary)]"
                        : "border-[var(--color-border-strong)] text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                    }`}
                    onClick={() => setMemoryView(view)}
                  >
                    {view === "list" ? "Memories" : "Graph"}
                  </button>
                ))}
              </div>

              {memoryView === "graph" ? (
                <MemoryGraphView />
              ) : (
                <>
              <div className="grid gap-2">
                <textarea
                  className="min-h-24 w-full resize-y rounded-[0.9rem] border border-[var(--color-border-strong)] bg-transparent px-3 py-2 text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
                  placeholder="Remember that deal names may contain intentional typos."
                  value={memoryDraft}
                  onChange={(event) => setMemoryDraft(event.target.value)}
                />
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <label className="text-[0.72rem] text-[var(--color-text-muted)]" htmlFor="memory-kind">
                      Kind
                    </label>
                    <select
                      id="memory-kind"
                      className="rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                      value={memoryKindDraft}
                      onChange={(event) => setMemoryKindDraft(event.target.value)}
                    >
                      {MEMORY_KINDS.map((k) => (
                        <option key={k} value={k}>
                          {k}
                        </option>
                      ))}
                    </select>
                  </div>
                  <div className="flex items-center gap-2">
                    {editingMemoryId ? (
                      <button
                        type="button"
                        className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                        onClick={() => {
                          setEditingMemoryId(null);
                          setMemoryDraft("");
                        }}
                      >
                        Cancel edit
                      </button>
                    ) : null}
                    <button
                      type="button"
                      className="rounded-full bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40"
                      disabled={!memoryDraft.trim() || isSavingMemory}
                      onClick={() => void saveMemory()}
                    >
                      {isSavingMemory ? "Saving..." : editingMemoryId ? "Save changes" : "Add memory"}
                    </button>
                  </div>
                </div>
              </div>

              {memoryError ? (
                <div className="rounded-[0.75rem] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger,#dc2626)]">
                  {memoryError}
                </div>
              ) : null}

              <div className="min-h-0 flex-1 overflow-y-auto pr-1">
                {isLoadingMemories ? (
                  <p className="py-4 text-[0.8125rem] text-[var(--color-text-muted)]">Loading memories...</p>
                ) : memories.length === 0 ? (
                  <p className="rounded-[0.9rem] border border-dashed border-[var(--color-border)] px-3 py-4 text-[0.8125rem] leading-[1.5] text-[var(--color-text-muted)]">
                    No memories yet. Add one manually or tell chat “remember this: ...”.
                  </p>
                ) : (
                  <div className="grid gap-2">
                    {memories.map((memory) => {
                      const retired = memory.retired_at != null;
                      const sourceLabel =
                        memory.source === "chat"
                          ? memory.origin === "auto"
                            ? "Auto-extracted from chat"
                            : "Saved from chat"
                          : memory.source === "proposed"
                            ? "Proposed"
                            : "Manual";
                      const learned = memory.learned_at
                        ? new Date(memory.learned_at * 1000).toLocaleDateString()
                        : null;
                      return (
                        <div
                          key={memory.id}
                          className={`rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3 ${retired ? "opacity-60" : ""}`}
                        >
                          <p className="whitespace-pre-wrap text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)]">
                            {memory.pinned ? <span title="Pinned">📌 </span> : null}
                            {memory.content}
                          </p>
                          <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
                            <span>
                              {memory.kind && memory.kind !== "fact" ? (
                                <span className="mr-1.5 rounded-full border border-[var(--color-border)] px-1.5 py-0.5">
                                  {memory.kind}
                                </span>
                              ) : null}
                              {sourceLabel}
                              {learned ? ` · learned ${learned}` : ""}
                              {retired ? " · retired" : ""}
                            </span>
                            <div className="flex items-center gap-3">
                              {retired ? (
                                <button
                                  type="button"
                                  className="hover:text-[var(--color-text-primary)]"
                                  onClick={() => void patchMemory(memory.id, { retired: false })}
                                >
                                  Restore
                                </button>
                              ) : (
                                <>
                                  <button
                                    type="button"
                                    className="hover:text-[var(--color-text-primary)]"
                                    onClick={() => void patchMemory(memory.id, { pinned: !memory.pinned })}
                                  >
                                    {memory.pinned ? "Unpin" : "Pin"}
                                  </button>
                                  <button
                                    type="button"
                                    className="hover:text-[var(--color-text-primary)]"
                                    onClick={() => {
                                      setEditingMemoryId(memory.id);
                                      setMemoryDraft(memory.content);
                                      setMemoryKindDraft(memory.kind ?? "fact");
                                    }}
                                  >
                                    Edit
                                  </button>
                                  <button
                                    type="button"
                                    className="hover:text-[var(--color-text-primary)]"
                                    title="Keep for audit, stop injecting into chats"
                                    onClick={() => void patchMemory(memory.id, { retired: true })}
                                  >
                                    Retire
                                  </button>
                                </>
                              )}
                              <button
                                type="button"
                                className="hover:text-[var(--color-danger)]"
                                onClick={() => void deleteMemory(memory.id)}
                              >
                                Delete
                              </button>
                            </div>
                          </div>
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
                </>
              )}
            </div>
          </div>
        ) : null}

        {confirmSummarize ? (
          <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
            <button
              aria-label="Close compact confirmation"
              className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
              type="button"
              onClick={() => setConfirmSummarize(false)}
            />
            <div className="relative z-10 w-full max-w-[26rem] rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
              <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
                Compact this conversation?
              </h2>
              <p className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
                Long conversations get expensive and can hit the model&apos;s context
                window. Compacting replaces earlier turns with a short summary so
                the next turn stays affordable and fits. The originals collapse
                below a banner — you can expand them again anytime.
              </p>
              <div className="flex items-center justify-end gap-2">
                <button
                  type="button"
                  className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                  onClick={() => setConfirmSummarize(false)}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  className="rounded-full bg-[var(--color-primary)] px-4 py-2 text-[0.8125rem] font-medium text-white transition hover:opacity-90"
                  onClick={() => {
                    setConfirmSummarize(false);
                    void summarizeConversation();
                  }}
                >
                  Compact
                </button>
              </div>
            </div>
          </div>
        ) : null}

        {pendingDeleteConversation ? (
          <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
            <button
              aria-label="Close delete confirmation"
              className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
              type="button"
              onClick={() => setPendingDeleteConversation(null)}
            />

            <div className="relative z-10 w-full max-w-[25rem] rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
              <div className="mb-4 grid gap-2">
                <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">Delete chat?</h2>
                <p className="text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
                  Are you sure you want to delete <strong>&quot;{pendingDeleteConversation.title}&quot;</strong>?
                </p>
              </div>

              <div className="flex items-center justify-end gap-2">
                <button
                  className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                  type="button"
                  onClick={() => setPendingDeleteConversation(null)}
                >
                  Cancel
                </button>
                <button
                  className="rounded-full bg-[var(--color-primary)] px-4 py-2 text-[0.8125rem] font-medium text-white transition hover:opacity-90"
                  type="button"
                  onClick={() => void confirmDeleteConversation()}
                >
                  Delete
                </button>
              </div>
            </div>
          </div>
        ) : null}

        <main className="flex min-h-0 min-w-0 flex-col overflow-hidden" suppressHydrationWarning>
          {/* Shared top header (#169): "Chat" on the left, the chat's header
              controls (search, shortcuts, memories, details) right-aligned —
              the same bordered bar the Operations Center renders. */}
          <PageTopBar
            title="Chat"
            onMenu={() => setSidebarOpen(true)}
            actions={
              <>
                {/* Unified page-header search (#169): an icon-only button, inline
                    with the other header icons, that opens the full-text search
                    overlay (also bound to ⌘K). The rail's own "Search chats…" input
                    is a separate local title filter — both are unchanged. */}
                <button
                  aria-label="Search conversations"
                  className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                  title={`Search conversations (${searchShortcut})`}
                  type="button"
                  onClick={() => setSearchOpen(true)}
                >
                  <Icon name="search" className="size-5" />
                </button>
                <button
                  aria-label="Keyboard shortcuts"
                  className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                  title="Keyboard shortcuts (?)"
                  data-testid="shortcuts-button"
                  type="button"
                  onClick={() => setShortcutsOpen(true)}
                >
                  <Icon name="info" className="size-5" />
                </button>
                <button
                  aria-label="Projects"
                  className="relative inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                  title="Projects"
                  type="button"
                  onClick={() => setProjectsOpen(true)}
                >
                  <Icon name="grid" className="size-5" />
                </button>
                <button
                  aria-label="Manage memories"
                  className="relative inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                  title="Manage memories"
                  type="button"
                  onClick={openMemoryManager}
                >
                  <Icon name="brain" className="size-5" />
                </button>
                <button
                  aria-label={showStats ? "Hide details (thinking, stats, tool calls)" : "Show details (thinking, stats, tool calls)"}
                  aria-pressed={showStats}
                  // Color stays muted in both states — the icon swap is
                  // the affordance the user keys off, not an accent
                  // highlight. Mirrors the sun/moon theme toggle next door.
                  className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                  title={showStats ? "Hide details" : "Show details"}
                  type="button"
                  onClick={toggleShowStats}
                >
                  <span className="relative size-4" aria-hidden="true">
                    <Icon
                      name="sparkles"
                      className={[
                        "absolute inset-0 size-4 transition duration-200",
                        showStats
                          ? "rotate-12 scale-[0.86] opacity-0"
                          : "rotate-0 scale-100 opacity-100",
                      ].join(" ")}
                    />
                    <Icon
                      name="info"
                      className={[
                        "absolute inset-0 size-4 transition duration-200",
                        showStats
                          ? "rotate-0 scale-100 opacity-100"
                          : "-rotate-12 scale-[0.86] opacity-0",
                      ].join(" ")}
                    />
                  </span>
                </button>
              </>
            }
          />

          {/* Active conversation name (#169): a heading at the top of the chat
              content area, left-aligned to the "Chat" header label, carrying the
              existing rename-on-click affordance + the lockdown badge. A new/empty
              chat (no active conversation) shows no name — the empty-state hero in
              the transcript stands in. */}
          {activeConversationId ? (
            <div className="flex min-w-0 items-center gap-2 px-4 pt-3 sm:px-6">
              {renamingTitleDraft !== null ? (
                <input
                  autoFocus
                  aria-label="Rename chat"
                  disabled={isSavingTitle}
                  value={renamingTitleDraft}
                  onChange={(e) => setRenamingTitleDraft(e.target.value)}
                  onFocus={(e) => e.currentTarget.select()}
                  onBlur={async () => {
                    const draft = renamingTitleDraft;
                    if (!activeConversationId || draft === null) return;
                    const trimmed = draft.trim();
                    if (!trimmed || trimmed === title) {
                      setRenamingTitleDraft(null);
                      return;
                    }
                    setIsSavingTitle(true);
                    await renameConversation(activeConversationId, trimmed);
                    setIsSavingTitle(false);
                    setRenamingTitleDraft(null);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      e.currentTarget.blur();
                    } else if (e.key === "Escape") {
                      e.preventDefault();
                      setRenamingTitleDraft(null);
                    }
                  }}
                  className="min-w-0 flex-1 rounded-md border border-[var(--color-accent)] bg-transparent py-1 pl-0 pr-1.5 text-[1.0625rem] font-semibold text-[var(--color-text-primary)] outline-none sm:text-[1.25rem]"
                />
              ) : (
                <h2 className="min-w-0">
                  <button
                    type="button"
                    title="Click to rename"
                    onClick={() => setRenamingTitleDraft(title)}
                    className="block min-w-0 max-w-full cursor-text truncate rounded-md py-1 pr-1.5 text-left text-[1.0625rem] font-semibold text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] sm:text-[1.25rem]"
                  >
                    {title}
                  </button>
                </h2>
              )}
              {isLockdown ? (
                <span
                  className="inline-flex shrink-0 items-center gap-1 rounded-full border border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 px-2 py-0.5 text-[0.6875rem] font-medium text-[var(--color-accent)]"
                  title="Lockdown chat — sandboxed container, restricted models"
                >
                  <Icon name="lock" className="size-3" />
                  <span className="hidden sm:inline">Lockdown</span>
                </span>
              ) : null}
            </div>
          ) : null}

          {/* Chat content — transcript scrolls, composer pins to the bottom.
              grid-cols-[minmax(0,1fr)] clamps the single column to the container
              width so rich content can't bleed past the right edge on mobile;
              min-w-0 descendants still scroll wide children internally. */}
          <div className="grid min-h-0 min-w-0 flex-1 grid-cols-[minmax(0,1fr)] grid-rows-[minmax(0,1fr)_auto] gap-2 overflow-hidden px-3 pb-3 pt-2 sm:gap-3 sm:px-6 sm:pb-5 lg:px-8 xl:px-10">

          <ChatTranscript
            conversationRef={conversationRef}
            streamEndRef={streamEndRef}
            promptRef={promptRef}
            isLoadingHistory={isLoadingHistory}
            isLockdown={isLockdown}
            messages={messages}
            pills={pills}
            activePillId={activePillId}
            setActivePillId={setActivePillId}
            submitPrompt={submitPrompt}
            setPrompt={setPrompt}
            isSummarizing={isSummarizing}
            summarizeStartedAt={summarizeStartedAt}
            summarizeStream={summarizeStream}
            summarizeError={summarizeError}
            summaryIndex={summaryIndex}
            summaryExpanded={summaryExpanded}
            setSummaryExpanded={setSummaryExpanded}
            setConfirmSummarize={setConfirmSummarize}
            showStats={showStats}
            crossfadingMessageIds={crossfadingMessageIds}
            currentConvKey={currentConvKey}
            realConvId={realConvId}
            isStreaming={isStreaming}
            lastUserMessageId={lastUserMessageId}
            lastAssistantMessageId={lastAssistantMessageId}
            selectedModel={selectedModel}
            patchAssistantMessage={patchAssistantMessage}
            resendUserMessage={resendUserMessage}
            retryLastUserMessage={retryLastUserMessage}
            regenerateLastAssistant={regenerateLastAssistant}
            branchFromMessage={branchFromMessage}
            loadMemories={loadMemories}
            setSelectedModel={setSelectedModel}
            setModelPickerOpen={setModelPickerOpen}
            setModelSearchQuery={setModelSearchQuery}
            loadRankedModels={loadRankedModels}
            loadCatalogModels={loadCatalogModels}
          />

          <section className="relative z-10 pb-[calc(env(safe-area-inset-bottom,0px)+0.35rem)] sm:pb-4">
            {showJumpToLatest ? (
              // Anchored to the composer section's TOP edge, not the
              // viewport bottom, so it always sits just above the form
              // regardless of how the toolbar wraps. The earlier
              // "viewport bottom + 6.25rem" approach overlapped the
              // textarea on iPhone-class widths once the toolbar
              // wrapped onto a second row.
              <div className="pointer-events-none absolute -top-12 right-3 z-20 flex justify-end sm:-top-14 sm:right-6 lg:right-8">
                <button
                  aria-label="Jump to latest"
                  className="pointer-events-auto inline-flex size-11 items-center justify-center rounded-full border border-[var(--color-border-strong)] bg-[var(--gradient-surface-elevated)] text-[var(--color-text-primary)] shadow-[var(--shadow-md)] backdrop-blur transition hover:border-[var(--color-accent)] hover:text-[var(--color-white)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                  type="button"
                  onClick={jumpToLatest}
                >
                  <svg aria-hidden="true" viewBox="0 0 20 20" className="size-4" fill="none" stroke="currentColor" strokeWidth="1.8">
                    <path d="M10 4v10" strokeLinecap="round" />
                    <path d="m5.5 10.5 4.5 4.5 4.5-4.5" strokeLinecap="round" strokeLinejoin="round" />
                  </svg>
                </button>
              </div>
            ) : null}
            <div className="pointer-events-none absolute inset-x-0 -top-16 h-16 bg-[var(--sticky-fade)]" />
            <div className="mx-auto mb-1 w-full max-w-[52rem] px-1 sm:mb-1.5 sm:px-0">
              {showStats ? (
                <ConversationTotalsChip messages={messages} usage={contextUsage} />
              ) : null}
            </div>
            {modelError ? (
              <div
                role="alert"
                className="mx-auto mb-1 w-full max-w-[52rem] rounded-[0.9rem] border border-[var(--color-danger)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger)] sm:mb-1.5"
              >
                {modelError.message}{" "}
                <a
                  href={modelError.modelsUrl}
                  target="_blank"
                  rel="noreferrer noopener"
                  className="underline"
                >
                  Browse affordable models
                </a>
                .
              </div>
            ) : null}
            <Composer
              prompt={prompt}
              setPrompt={setPrompt}
              promptPlaceholder={promptPlaceholder}
              promptRef={promptRef}
              submitPrompt={submitPrompt}
              isStreaming={isStreaming}
              isUploadingAttachments={isUploadingAttachments}
              isDraggingOver={isDraggingOver}
              setIsDraggingOver={setIsDraggingOver}
              dragCounterRef={dragCounterRef}
              fileInputRef={fileInputRef}
              addAttachmentFiles={addAttachmentFiles}
              pendingAttachments={pendingAttachments}
              attachmentError={attachmentError}
              removePendingAttachment={removePendingAttachment}
              spreadsheetNudge={spreadsheetNudge}
              setSpreadsheetNudgeDismissed={setSpreadsheetNudgeDismissed}
              personas={personas}
              selectedPersona={selectedPersona}
              setSelectedPersona={setSelectedPersona}
              personaPickerOpen={personaPickerOpen}
              setPersonaPickerOpen={setPersonaPickerOpen}
              personaPickerRef={personaPickerRef}
              selectedModel={selectedModel}
              setSelectedModel={setSelectedModel}
              modelError={modelError}
              modelPickerOpen={modelPickerOpen}
              setModelPickerOpen={setModelPickerOpen}
              modelPickerRef={modelPickerRef}
              modelInputRef={modelInputRef}
              modelSearchQuery={modelSearchQuery}
              setModelSearchQuery={setModelSearchQuery}
              filteredRankedModels={filteredRankedModels}
              isLoadingRankedModels={isLoadingRankedModels}
              isLoadingCatalog={isLoadingCatalog}
              loadRankedModels={loadRankedModels}
              loadCatalogModels={loadCatalogModels}
              skills={skills}
              mcpServers={mcpServers}
              mcpPickerOpen={mcpPickerOpen}
              setMcpPickerOpen={setMcpPickerOpen}
              mcpPickerRef={mcpPickerRef}
              isLoadingMcpServers={isLoadingMcpServers}
              loadMcpServerCatalog={loadMcpServerCatalog}
              toggleMcpServer={toggleMcpServer}
              activeConversationId={activeConversationId}
              messages={messages}
              contextUsage={contextUsage}
              isSummarizing={isSummarizing}
              compactToastVisible={compactToastVisible}
              setConfirmSummarize={setConfirmSummarize}
              activeConversationIdRef={activeConversationIdRef}
              abortControllersRef={abortControllersRef}
              isPendingKey={isPendingKey}
            />
          </section>
          </div>
        </main>
      </div>
    </div>
  );
}
