"use client";

// Composer — the chat message-input form extracted from ChatExperience
// (issue #169 decomposition, slice 7). This is a *presentational* component:
// it owns no turn/send state machine of its own. Every value, setter, ref,
// and callback it needs is threaded in as a prop from ChatExperience, which
// keeps owning the per-conversation composer state, the upload pipeline, and
// the streaming turn loop. Container AND toolbar follow the unified-shell
// design's .composer: 53rem, --composer-surface, --radius-xl, the
// focus-fading keyboard hint, the sealed strip, and the design toolbar —
// model chip + listbox popover (with a search field the design doesn't have,
// preserving type-to-search + free slug entry), .composer-div divider,
// .tool-btn icon buttons (persona · attach · tools-with-badge · context
// ring), and the circular arrow send button. The behavioral contracts the
// specs drive — placeholder format, aria-labels ("Attach files", "Model",
// "Connectors", "Send message"), textarea-first, send flow,
// Enter-vs-Shift+Enter, attachments, model/persona/MCP pickers, Stop — are
// unchanged.
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import { useEffect, useMemo, useRef, useState } from "react";
import { PENDING_CONV_KEY } from "./workspaceHref";
import { conversationApiUrl } from "@/app/lib/conversationApiUrl";
import { Icon } from "./Icon";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import {
  ContextRing,
  ModelValidationBadge,
  NewModelBadge,
  PendingAttachmentChip,
  type PendingAttachment,
} from "./ChatChips";
import {
  currentAdvancedModel,
  currentAdvancedModelLabel,
  currentDefaultModel,
  tierForModel,
} from "@/app/lib/modelAliases";
import { ModelCostIndicator } from "@/app/shared/ui/ModelCostIndicator";
import { compactModelLabel } from "@/app/shared/lib/models";
import type { ModelPrices } from "@/app/shared/lib/modelCost";
import type { ContextUsage } from "@/app/lib/contextUsage";
import type { NudgeDecision } from "@/app/lib/spreadsheetNudge";
import type { Message } from "./history";
import type { MCPServerInfo, RankedModel } from "./chat-experience";
import {
  completeSkill,
  filterSkills,
  skillSlashQuery,
  type SkillInfo,
} from "./skillSlash";
import { PromptLibrary } from "@/app/shared/ui/PromptLibrary";
import { ToolsLibrary } from "./ToolsLibrary";
import { ToolsPopover } from "./ToolsPopover";
import { PatrolPopover, type CreatePatrolInput } from "./PatrolBanner";
import { countOn, orderedCategories, type PickerCategory } from "./toolsPicker";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { Link } from "@/app/lib/Link";
import {
  formatBundlePersona,
  isUserPersonaRef,
  personaLabel,
  type UserPersonaOption,
} from "./userPersonas";

// isNewlyReleased was a module-level helper in chat-experience; its only
// caller was the composer's model-picker rows, so it moves here verbatim
// (the original is removed from chat-experience to keep one definition).
//
// Pill threshold. Models listed on OpenRouter within this window get
// the "✨ new" badge in the picker. 14 days is short enough that the
// badge means something but long enough that mid-week releases stay
// flagged through the following weekend. Tuneable.
const NEW_MODEL_WINDOW_DAYS = 14;

function isNewlyReleased(createdSeconds: number | undefined): boolean {
  if (!createdSeconds || createdSeconds <= 0) return false;
  const ageDays = (Date.now() / 1000 - createdSeconds) / 86400;
  return ageDays >= 0 && ageDays < NEW_MODEL_WINDOW_DAYS;
}

// looksLikeCode is a cheap heuristic used by the paste handler to decide
// whether to surface the "Format as code" nudge. It's deliberately
// permissive: a false positive just shows a dismissible hint, while a
// false negative only means the user has to wrap the snippet by hand.
// Short pastes (<40 chars) never trigger — almost any single line that
// short is plain prose. The checks look for indentation patterns and the
// punctuation/keywords that are rare in natural language but ubiquitous
// in code.
function looksLikeCode(text: string): boolean {
  if (text.length < 40) return false;
  return (
    /^ {4}/m.test(text) || // 4-space indent (Markdown-style code block)
    /^\t/m.test(text) || // tab indent
    /[{};=>]/.test(text) || // common code punctuation
    /\bfunction\b|\bconst\b|\bimport\b|\bdef\b|\bclass\b/.test(text)
  );
}

// localStorage key for the "Send on Enter" vs "Send on Ctrl/Cmd+Enter"
// preference. Read once at composer mount; written whenever the toggle
// pill next to the Send button is clicked. Defaults to "enter" (the
// muscle-memory default for every major chat UI).
const SEND_KEY_STORAGE = "fleet.sendKey";
const CODE_NUDGE_TIMEOUT_MS = 6000;

// ── Design toolbar building blocks (unified shell .composer-toolbar) ────────
// The design's .tool-btn: a 1.95rem radius-md icon button that tints with the
// accent on hover; TOOL_BTN_ACTIVE marks an engaged control (a persona
// picked, send-on-Enter on).
const TOOL_BTN =
  "relative inline-flex h-[1.95rem] w-[1.95rem] shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition hover:bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40";
// The tools trigger while its popover is open: the design's neutral pressed
// fill (chip border) rather than the accent tint the other toggles use.
const TOOLS_BTN_OPEN =
  "bg-[var(--color-pill-border)] text-[var(--color-text-secondary)] hover:bg-[var(--color-pill-border)]";
const TOOL_BTN_ACTIVE =
  "bg-[color-mix(in_srgb,var(--color-accent)_14%,transparent)] text-[var(--color-accent)] hover:bg-[color-mix(in_srgb,var(--color-accent)_22%,transparent)]";

// The design's .composer-pop: the anchored popover surface every composer
// menu shares. Mobile pins it to the viewport (the anchored layout could
// push it off-screen behind the keyboard); ≥sm anchors above the trigger.
// grid-cols-[minmax(0,1fr)]: an implicit `auto` grid column sizes to the
// widest row's max-content, so a long model name would push every row wider
// than the popover and force a horizontal scrollbar — minmax(0,1fr) pins the
// column to the popover width and lets row content shrink/wrap instead.
// Width lives on the per-menu variants: the model listbox is wider than the
// design's 15.5rem so live model names + a badge fit on one line (they wrap,
// never clip, if they outgrow even that).
const COMPOSER_POP_BASE =
  "fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 grid grid-cols-[minmax(0,1fr)] gap-[0.1rem] rounded-[0.75rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-[0.3rem] shadow-[var(--shadow-md)] motion-safe:animate-pop-up sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.55rem)] sm:left-0";
const COMPOSER_POP_WIDE = `${COMPOSER_POP_BASE} sm:w-[19rem]`;

// The design's .pop-row / .pop-title type ramp, for the composer's simple
// menus (the tools popover has its own denser rows, ToolsPopover).
const POP_ROW_BASE =
  "flex w-full items-center justify-between gap-2 rounded-[0.5rem] text-left text-[0.82rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";
const POP_ROW = `${POP_ROW_BASE} px-[0.6rem] py-[0.45rem]`;
const POP_ROW_SELECTED =
  "bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)]";
const POP_TITLE = "text-[0.82rem] font-medium text-[var(--color-text-primary)]";

export type ComposerProps = {
  /**
   * Dashboard view (docs/DASHBOARD-VIEW.md): whether it is on, and the
   * toggle the More options menu offers. Omitted: no menu row.
   */
  dashboardView?: boolean;
  onToggleDashboardView?: () => void;
  // Textarea value + draft handling
  prompt: string;
  setPrompt: Dispatch<SetStateAction<string>>;
  promptPlaceholder: string;
  promptRef: RefObject<HTMLTextAreaElement | null>;
  submitPrompt: (submittedPrompt: string) => void | Promise<void | boolean>;

  // Sealed (lockdown) view: accent-mixed border + the explainer strip along
  // the composer's top edge. True for an active lockdown conversation, a
  // pending "new sealed chat", and lockdown-only servers.
  sealed: boolean;

  // Per-conversation auto-approve toggle: pre-approves executable critical
  // tools in this chat without parking on an approval card.
  isAutoApprove?: boolean;
  toggleAutoApprove?: () => void;
  isConversationOwner?: boolean;

  // Turn / upload gating
  isStreaming: boolean;
  isUploadingAttachments: boolean;

  // Drag-and-drop attach
  isDraggingOver: boolean;
  setIsDraggingOver: Dispatch<SetStateAction<boolean>>;
  dragCounterRef: RefObject<number>;
  fileInputRef: RefObject<HTMLInputElement | null>;
  addAttachmentFiles: (files: FileList | null) => void;

  // Pending attachment chips
  pendingAttachments: PendingAttachment[];
  attachmentError: string | null;
  removePendingAttachment: (clientId: string) => void;
  // Informational "large upload — this may take a while" banner copy,
  // null when the queued total is small enough not to matter.
  uploadSizeWarning: string | null;

  // Spreadsheet nudge
  spreadsheetNudge: NudgeDecision;
  setSpreadsheetNudgeDismissed: Dispatch<SetStateAction<boolean>>;

  // Persona picker: the workspace (bundle) roster plus the user's own
  // personas (Settings → Personas), selected by their "user:<name>" ref.
  personas: string[];
  defaultPersona?: string;
  userPersonas?: UserPersonaOption[];
  selectedPersona: string;
  setSelectedPersona: Dispatch<SetStateAction<string>>;
  personaPickerOpen: boolean;
  setPersonaPickerOpen: Dispatch<SetStateAction<boolean>>;
  personaPickerRef: RefObject<HTMLDivElement | null>;

  // Model picker
  selectedModel: string;
  setSelectedModel: Dispatch<SetStateAction<string>>;
  // Ends a recovery chain when the user presses Stop (see the Stop handler).
  cancelRecovery: (convId: string) => void;
  // Display label for the chip: the tier alias ("default"/"advanced") or the
  // catalog display name for a known slug — the same string the menu row
  // shows — falling back to the raw slug/typed text. Resolved by
  // ChatExperience, which owns the catalog.
  selectedModelLabel: string;
  // OpenRouter prices for the selected slug, driving the "$ … $$$$" cost tier
  // on the chip. null whenever the slug has no catalog pricing (workspace
  // provider, half-typed custom slug) — the chip then shows no tier.
  selectedModelPrices: ModelPrices | null;
  // The selected model is an operator pin marked free: the chip reads "Free".
  selectedModelFree?: boolean;
  modelError: { message: string; modelsUrl: string } | null;
  modelPickerOpen: boolean;
  setModelPickerOpen: Dispatch<SetStateAction<boolean>>;
  modelPickerRef: RefObject<HTMLDivElement | null>;
  modelInputRef: RefObject<HTMLInputElement | null>;
  modelSearchQuery: string;
  setModelSearchQuery: Dispatch<SetStateAction<string>>;
  filteredRankedModels: RankedModel[];
  isLoadingRankedModels: boolean;
  isLoadingCatalog: boolean;
  loadRankedModels: () => void | Promise<void>;
  loadCatalogModels: () => void | Promise<void>;

  // Bundle skill roster for the "/" autocomplete (#513). Fetched once by
  // ChatExperience; empty when the bundle ships no skills (popover never opens).
  skills: SkillInfo[];

  // MCP (optional tools) picker
  mcpServers: MCPServerInfo[];
  mcpPickerOpen: boolean;
  setMcpPickerOpen: Dispatch<SetStateAction<boolean>>;
  mcpPickerRef: RefObject<HTMLDivElement | null>;
  isLoadingMcpServers: boolean;
  loadMcpServerCatalog: (conversationId: string) => void | Promise<void>;
  toggleMcpServer: (
    conversationId: string | null,
    name: string,
  ) => void | Promise<void>;
  // "All on" / "All off": the named connectors (the ones the popover or
  // library is showing) at once; rows with no switch are never part of it.
  setMcpServersEnabled: (
    conversationId: string | null,
    names: string[],
    enabled: boolean,
  ) => void | Promise<void>;
  // The picker's category headings, in order ([] = the built-in fallback).
  mcpCategories: PickerCategory[];
  // The deployment's name ("Elcano"): the default account's label.
  appName: string;
  // Shows the library's admin-only links (visibility only).
  isAdmin: boolean;
  // Credential-seat override for one server in this conversation (#988);
  // "" = back to the user's default seat.
  setMcpServerAccount: (
    conversationId: string | null,
    name: string,
    account: string,
  ) => void | Promise<void>;

  // Context ring / compaction
  activeConversationId: string | null;
  messages: Message[];
  contextUsage: ContextUsage | null;
  isSummarizing: boolean;
  compactToastVisible: boolean;
  setConfirmSummarize: Dispatch<SetStateAction<boolean>>;

  // Stop control
  activeConversationIdRef: RefObject<string | null>;
  abortControllersRef: RefObject<Map<string, AbortController>>;
  isPendingKey: (key: string | null) => boolean;

  // In-chat recurring patrols & one-shot timers
  onStartPatrol?: (input: CreatePatrolInput) => boolean | void | Promise<boolean | void>;
  activePatrolCount?: number;

  // Sharing entry point right in the composer from the start of the chat.
  onOpenShare?: () => void;
  shareConversation?: {
    team_visible?: boolean;
    share_token?: string;
    multiplayer?: boolean;
  } | null;
  shareTeam?: string;
  shareBusy?: boolean;

  // Memories & turn details (relocated from top header into More options).
  onOpenMemories?: () => void;
  showDetails?: boolean;
  onToggleShowDetails?: () => void;
};

export function Composer({
  dashboardView = false,
  onToggleDashboardView,
  prompt,
  setPrompt,
  promptPlaceholder,
  promptRef,
  submitPrompt,
  sealed,
  isAutoApprove = false,
  toggleAutoApprove,
  isConversationOwner = true,
  isStreaming,
  isUploadingAttachments,
  isDraggingOver,
  setIsDraggingOver,
  dragCounterRef,
  fileInputRef,
  addAttachmentFiles,
  pendingAttachments,
  attachmentError,
  uploadSizeWarning,
  removePendingAttachment,
  spreadsheetNudge,
  setSpreadsheetNudgeDismissed,
  personas,
  defaultPersona,
  userPersonas = [],
  selectedPersona,
  setSelectedPersona,
  personaPickerOpen,
  setPersonaPickerOpen,
  personaPickerRef,
  selectedModel,
  setSelectedModel,
  cancelRecovery,
  selectedModelLabel,
  selectedModelPrices,
  selectedModelFree,
  modelError,
  modelPickerOpen,
  setModelPickerOpen,
  modelPickerRef,
  modelInputRef,
  modelSearchQuery,
  setModelSearchQuery,
  filteredRankedModels,
  isLoadingRankedModels,
  isLoadingCatalog,
  loadRankedModels,
  loadCatalogModels,
  skills,
  mcpServers,
  mcpPickerOpen,
  setMcpPickerOpen,
  mcpPickerRef,
  isLoadingMcpServers,
  loadMcpServerCatalog,
  toggleMcpServer,
  setMcpServersEnabled,
  mcpCategories,
  appName,
  isAdmin,
  setMcpServerAccount,
  activeConversationId,
  messages,
  contextUsage,
  isSummarizing,
  compactToastVisible,
  setConfirmSummarize,
  activeConversationIdRef,
  abortControllersRef,
  isPendingKey,
  onStartPatrol,
  activePatrolCount = 0,
  onOpenShare,
  shareConversation,
  shareBusy = false,
  onOpenMemories,
  showDetails = false,
  onToggleShowDetails,
}: ComposerProps) {
  // Multi-line composer UX (issue #315):
  // - `isComposerFocused` drives the keyboard-hint line under the composer:
  //   it fades in while the textarea has focus and out on blur (the design's
  //   .composer-hint), so the hint is present exactly while the keys it
  //   documents are live.
  // - `sendOnEnter` is the localStorage-backed send-key preference. Read
  //   once at mount; the toggle pill next to Send flips it and persists.
  // - `showCodeNudge` is the transient "Format as code" banner fired when
  //   a paste looks like source. Auto-dismisses after CODE_NUDGE_TIMEOUT_MS.
  const [isComposerFocused, setIsComposerFocused] = useState(false);
  const [showCodeNudge, setShowCodeNudge] = useState(false);
  // Skill "/" autocomplete (#513). The popover is fully derived from the
  // draft: it opens while the draft is a bare "/<token>" with matching bundle
  // skills, and closes the moment whitespace follows the token (the user is
  // typing arguments) or the leading "/" goes away. `skillIndex` is the
  // keyboard-highlighted row (reset by the textarea onChange on every edit —
  // arrow keys don't fire onChange, so navigation survives); it is clamped at
  // render so a shrinking match list can't strand the highlight.
  // `skillPopoverDismissed` is the Esc latch; onChange re-arms it whenever an
  // edit leaves the slash context, so a later "/" reopens the popover. Both
  // resets live in event handlers, not effects.
  const [skillIndex, setSkillIndex] = useState(0);
  const [skillPopoverDismissed, setSkillPopoverDismissed] = useState(false);
  const skillQuery = skillSlashQuery(prompt);
  const skillMatches =
    skillQuery === null ? [] : filterSkills(skills, skillQuery);
  const skillHighlight = Math.min(
    skillIndex,
    Math.max(skillMatches.length - 1, 0),
  );
  const skillPopoverOpen =
    skillMatches.length > 0 &&
    !skillPopoverDismissed &&
    !isStreaming &&
    !shareBusy;
  const [sendOnEnter, setSendOnEnter] = useState<boolean>(() => {
    if (typeof window === "undefined") return true;
    try {
      return (localStorage.getItem(SEND_KEY_STORAGE) ?? "enter") === "enter";
    } catch {
      return true;
    }
  });
  // Keyboard support for the model listbox (the design's .composer-pop):
  // ArrowUp/Down move this highlight from the search field, Enter picks it,
  // Escape closes and returns focus to the chip. Clamped at use so a
  // shrinking match list can't strand it.
  const [modelHighlight, setModelHighlight] = useState(0);
  // Popover triggers, for the Esc/selection focus-return contract.
  const modelChipRef = useRef<HTMLButtonElement | null>(null);
  const personaButtonRef = useRef<HTMLButtonElement | null>(null);
  const mcpButtonRef = useRef<HTMLButtonElement | null>(null);
  const patrolButtonRef = useRef<HTMLButtonElement | null>(null);
  const patrolPopoverRef = useRef<HTMLDivElement | null>(null);
  const [patrolPopoverOpen, setPatrolPopoverOpen] = useState(false);
  const moreMenuRef = useRef<HTMLDivElement | null>(null);
  const moreButtonRef = useRef<HTMLButtonElement | null>(null);
  const [moreMenuOpen, setMoreMenuOpen] = useState(false);
  const openPromptLibraryRef = useRef<(() => void) | null>(null);
  // The Tools library modal (null = closed), optionally opened on one
  // connection from a dimmed popover row.
  const [toolsLibrary, setToolsLibrary] = useState<{ selected?: string } | null>(null);
  const pickerCategories = useMemo(() => orderedCategories(mcpCategories), [mcpCategories]);
  // The wrench badge: connections on in this chat, always-on included. It
  // holds the count from when the popover opened while the popover or
  // library is open and catches up when they close, so it never changes
  // under the pointer mid-toggle.
  const liveToolsOn = countOn(mcpServers);
  const [heldToolsOn, setHeldToolsOn] = useState(liveToolsOn);
  const toolsBadge = mcpPickerOpen || toolsLibrary ? heldToolsOn : liveToolsOn;

  const openModelPicker = () => {
    setModelPickerOpen(true);
    setModelSearchQuery("");
    setModelHighlight(0);
    void loadRankedModels();
    void loadCatalogModels();
  };
  const closeModelPicker = (returnFocus: boolean) => {
    setModelPickerOpen(false);
    setModelSearchQuery("");
    if (returnFocus) modelChipRef.current?.focus();
  };

  // Persist the send-key preference whenever it changes. Wrapped in a
  // try/catch because localStorage can throw in private-browsing modes
  // (Safari) and we don't want a settings toggle to take down the chat.
  useEffect(() => {
    try {
      localStorage.setItem(
        SEND_KEY_STORAGE,
        sendOnEnter ? "enter" : "ctrl+enter",
      );
    } catch {
      /* private mode / quota — preference stays session-only, which is fine */
    }
  }, [sendOnEnter]);

  // Auto-dismiss the code-paste nudge after CODE_NUDGE_TIMEOUT_MS. Each
  // appearance re-arms the timer; dismissal (click or ✕) clears state
  // directly and the cleanup here is a no-op once it's already false.
  useEffect(() => {
    if (!showCodeNudge) return;
    const timer = window.setTimeout(
      () => setShowCodeNudge(false),
      CODE_NUDGE_TIMEOUT_MS,
    );
    return () => window.clearTimeout(timer);
  }, [showCodeNudge]);

  // The model picker's search field must take focus the moment the picker
  // opens — that is the whole point of the popover (type to search, Esc to
  // leave). It used to get there via autoFocus, which is a mount-time
  // side effect with no stated cause: it fires whenever the node happens to
  // mount, and a screen-reader user gets moved without having asked. Driving
  // it from `modelPickerOpen` instead ties the focus move to the user's own
  // action of opening the picker, and keeps the field unfocused in every other
  // case. Same UX, explicit cause.
  useEffect(() => {
    if (modelPickerOpen && !isStreaming) modelInputRef.current?.focus();
  }, [modelPickerOpen, isStreaming, modelInputRef]);

  useEffect(() => {
    if (!personaPickerOpen) return;
    const selectedOption = personaPickerRef.current?.querySelector<HTMLElement>(
      '[role="option"][aria-selected="true"], [role="option"]',
    );
    (selectedOption ?? personaButtonRef.current)?.focus();
  }, [personaPickerOpen, personaPickerRef]);

  useEffect(() => {
    if (!moreMenuOpen) return;
    const firstItem = moreMenuRef.current?.querySelector<HTMLElement>(
      '[role="menu"] [role^="menuitem"]:not([disabled])',
    );
    firstItem?.focus();
    const handleMouseDown = (event: MouseEvent) => {
      if (
        moreMenuRef.current &&
        !moreMenuRef.current.contains(event.target as Node)
      ) {
        setMoreMenuOpen(false);
      }
    };
    document.addEventListener("mousedown", handleMouseDown);
    return () => document.removeEventListener("mousedown", handleMouseDown);
  }, [moreMenuOpen]);

  return (
    <>
      <form
        // p-0 shell per the design: the textarea and toolbar own their
        // padding. The image: hint matters because --composer-surface
        // is a gradient — the un-hinted arbitrary-value form emits
        // background-color, which drops gradient values.
        className={`relative mx-auto w-full max-w-[53rem] rounded-[var(--radius-xl)] border bg-[image:var(--composer-surface)] shadow-[var(--shadow-md)] transition-colors ${
          isDraggingOver
            ? "border-[var(--color-accent)] ring-2 ring-[var(--color-accent)]/30"
            : sealed
              ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border))]"
              : "border-[var(--color-border)]"
        }`}
        // The form carries no accessible name, so it is not exposed as a
        // form landmark anyway; role="presentation" (explicitly allowed on
        // <form> by ARIA in HTML) says so, and lets the drop-zone handlers
        // below sit on the element the user actually drags onto without
        // claiming the composer is a widget a keyboard user can operate.
        // Drag-and-drop is pointer-only by nature; the keyboard/AT path to
        // the same action is the "Attach files" button in the toolbar.
        // Submission behavior is untouched — role never changes it.
        role="presentation"
        suppressHydrationWarning
        onSubmit={(event) => {
          event.preventDefault();
          void submitPrompt(prompt);
        }}
        onDragEnter={(event) => {
          event.preventDefault();
          dragCounterRef.current += 1;
          if (dragCounterRef.current === 1) setIsDraggingOver(true);
        }}
        onDragOver={(event) => {
          event.preventDefault();
        }}
        onDragLeave={() => {
          dragCounterRef.current -= 1;
          if (dragCounterRef.current === 0) setIsDraggingOver(false);
        }}
        onDrop={(event) => {
          event.preventDefault();
          dragCounterRef.current = 0;
          setIsDraggingOver(false);
          if (shareBusy) return;
          addAttachmentFiles(event.dataTransfer.files);
        }}
      >
        {isDraggingOver && (
          <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-[var(--radius-xl)] bg-[var(--color-accent)]/10">
            <span className="text-[0.8rem] font-medium text-[var(--color-accent)]">
              Drop to attach
            </span>
          </div>
        )}
        {/* Sealed strip along the composer's top edge (the design's
                  .composer-sealed-strip). First child of the p-0 shell, so it
                  runs edge-to-edge with its top corners following the
                  container radius. */}
        {sealed ? (
          <div className="flex items-center gap-[0.45rem] rounded-t-[calc(var(--radius-xl)-1px)] border-b border-[color-mix(in_srgb,var(--color-accent)_30%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] px-[1.05rem] py-[0.45rem] text-[0.75rem] text-[var(--color-text-secondary)]">
            <Icon
              name="lock"
              className="size-[0.8rem] shrink-0 text-[var(--color-accent)]"
            />
            <span>
              Sealed — your data and an approved model stay in this sandbox.
              Nothing leaves.
            </span>
          </div>
        ) : null}
        {/* Skill "/" autocomplete popover (#513). Anchored above the
                  composer like the persona/model dropdowns and reusing their
                  visual language. Rows complete to "/name " (via keyboard
                  Enter/Tab or click); the appended space keeps the caret
                  ready for arguments and closes the popover (whitespace ends
                  the slash context — see skillSlashQuery). */}
        {skillPopoverOpen ? (
          <div
            role="listbox"
            aria-label="Skills"
            className="motion-safe:animate-pop-up absolute bottom-[calc(100%+0.35rem)] left-0 z-30 w-full max-w-[24rem] overflow-hidden rounded-[0.9rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-surface-2)_96%,black)] shadow-[var(--shadow-lg)] backdrop-blur-xl"
          >
            <div className="max-h-72 overflow-y-auto py-1">
              {skillMatches.map((skill, i) => {
                const highlighted = i === skillHighlight;
                return (
                  <button
                    key={skill.name}
                    type="button"
                    role="option"
                    aria-selected={highlighted}
                    className={`flex w-full flex-col gap-0.5 px-3 py-2 text-left text-[0.74rem] transition hover:bg-[var(--color-overlay-soft)] ${
                      highlighted ? "bg-[var(--color-overlay-soft)]" : ""
                    }`}
                    // preventDefault on mousedown keeps the textarea
                    // focused, matching the persona/model pickers.
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => setPrompt(completeSkill(skill.name))}
                  >
                    <span
                      className={`font-medium ${
                        highlighted
                          ? "text-[var(--color-accent)]"
                          : "text-[var(--color-text-primary)]"
                      }`}
                    >
                      /{skill.name}
                    </span>
                    {skill.description ? (
                      <span className="line-clamp-2 text-[0.7rem] leading-snug text-[var(--color-text-muted)]">
                        {skill.description}
                      </span>
                    ) : null}
                  </button>
                );
              })}
            </div>
            <div className="border-t border-[var(--color-border)] px-3 py-1.5 text-[0.65rem] text-[var(--color-text-muted)]">
              ↑↓ to navigate · Enter/Tab to insert · Esc to dismiss
            </div>
          </div>
        ) : null}
        <label className="sr-only" htmlFor="promptInput">
          Message
        </label>
        <textarea
          id="promptInput"
          ref={promptRef}
          className="min-h-[68px] w-full resize-none overflow-y-auto bg-transparent px-[1.05rem] pt-[0.85rem] pb-[0.35rem] text-[16px] leading-[1.5] text-[var(--color-text-primary)] outline-none transition-[height] duration-fast placeholder:text-[var(--color-text-muted)] sm:text-[0.9rem]"
          placeholder={
            pendingAttachments.length > 0
              ? "Add a message to send your attachments…"
              : promptPlaceholder
          }
          rows={1}
          readOnly={shareBusy}
          suppressHydrationWarning
          value={prompt}
          onFocus={() => setIsComposerFocused(true)}
          onBlur={() => setIsComposerFocused(false)}
          onChange={(event) => {
            if (shareBusy) return;
            const value = event.target.value;
            setPrompt(value);
            // Every edit resets the skill-popover highlight to the top
            // row; an edit that leaves the slash context re-arms the Esc
            // latch so the next "/" reopens the popover.
            setSkillIndex(0);
            if (skillSlashQuery(value) === null)
              setSkillPopoverDismissed(false);
          }}
          onKeyDown={(event) => {
            // Skill "/" autocomplete steals its navigation keys while
            // open — most importantly Enter, which completes the
            // highlighted skill instead of sending, so accepting a
            // suggestion can never fire a half-typed message.
            if (skillPopoverOpen) {
              if (event.key === "ArrowDown") {
                event.preventDefault();
                setSkillIndex((skillHighlight + 1) % skillMatches.length);
                return;
              }
              if (event.key === "ArrowUp") {
                event.preventDefault();
                setSkillIndex(
                  (skillHighlight - 1 + skillMatches.length) %
                    skillMatches.length,
                );
                return;
              }
              if (event.key === "Enter" || event.key === "Tab") {
                event.preventDefault();
                const pick = skillMatches[skillHighlight];
                if (pick) setPrompt(completeSkill(pick.name));
                return;
              }
              if (event.key === "Escape") {
                event.preventDefault();
                setSkillPopoverDismissed(true);
                return;
              }
            }
            // Enter sends according to the user's send-key preference:
            //   - "enter" (default): bare Enter sends, Shift+Enter is
            //     a natural newline (textarea default).
            //   - "ctrl+enter": Enter is always a newline; only
            //     Cmd/Ctrl+Enter sends.
            // Touch devices are special-cased: their soft keyboards
            // send a bare Enter to insert a newline, so we never
            // intercept Enter there — submission stays on the Send
            // button. Cmd/Ctrl+Enter still works on a touch device
            // with a hardware keyboard attached (rare but cheap to
            // support).
            if (event.key !== "Enter") return;
            const isTouchDevice =
              typeof navigator !== "undefined" && navigator.maxTouchPoints > 0;
            const modifierSend = event.metaKey || event.ctrlKey;
            if (isTouchDevice && !modifierSend) return; // let the IME insert its newline
            const shouldSend = sendOnEnter ? !event.shiftKey : modifierSend;
            if (shouldSend) {
              event.preventDefault();
              if (!shareBusy) {
                void submitPrompt(prompt);
              }
            }
          }}
          onPaste={(event) => {
            if (shareBusy) {
              event.preventDefault();
              return;
            }
            // Pasting files / screenshots from clipboard runs
            // through the same addAttachmentFiles path as the
            // file-picker and drag-and-drop. Only intercept when
            // there are real files on the clipboard — plain-text
            // paste must still land in the textarea normally.
            // Modern browsers populate `files` for both browser
            // "Copy image" and OS-level screenshot pastes
            // (Cmd+Shift+Ctrl+4 on macOS, Win+Shift+S, etc.).
            const files = event.clipboardData?.files;
            if (files && files.length > 0) {
              event.preventDefault();
              addAttachmentFiles(files);
              return;
            }
            // Plain-text paste: let the browser insert it, then
            // (next tick, after React has committed the new value)
            // surface a "Format as code" nudge if it looks like
            // source. The autosize `useEffect` in ChatExperience
            // already grows the textarea in response to the prompt
            // state change, so no manual resize is needed here.
            const text = event.clipboardData?.getData("text/plain") ?? "";
            if (looksLikeCode(text)) {
              setTimeout(() => setShowCodeNudge(true), 0);
            }
          }}
        />

        {/* Code-paste nudge — surfaced when a paste looks like source
                  (see `looksLikeCode`). Auto-dismisses after
                  CODE_NUDGE_TIMEOUT_MS; the ✕ and "Format as code" actions
                  clear it immediately. "Format as code" wraps the entire
                  draft in a fenced block so the model renders it as code
                  rather than inlining the snippet as prose. */}
        {showCodeNudge ? (
          <div className="mx-[1.05rem] mt-1.5 flex items-center gap-2 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)]">
            <span>
              Pasted code? Wrap in triple backticks for better formatting.
            </span>
            <button
              type="button"
              disabled={shareBusy}
              className="font-medium text-[var(--color-accent)] hover:underline disabled:opacity-40"
              onClick={() => {
                if (shareBusy) return;
                setPrompt((p) => `\`\`\`\n${p}\n\`\`\``);
                setShowCodeNudge(false);
              }}
            >
              Format as code
            </button>
            <button
              type="button"
              aria-label="Dismiss code-format suggestion"
              className="text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
              onClick={() => setShowCodeNudge(false)}
            >
              <Icon name="close" className="size-3" />
            </button>
          </div>
        ) : null}

        {pendingAttachments.length > 0 || attachmentError ? (
          <div className="mx-[1.05rem] mb-2 flex flex-wrap items-center gap-1.5">
            {pendingAttachments.map((a) => (
              <PendingAttachmentChip
                key={a.clientId}
                attachment={a}
                onRemove={() => removePendingAttachment(a.clientId)}
                removalDisabled={isStreaming || isUploadingAttachments || shareBusy}
              />
            ))}
            {attachmentError ? (
              <span
                role="alert"
                className="basis-full rounded-[0.6rem] border border-[var(--color-danger)]/40 bg-[var(--color-danger)]/10 px-2.5 py-1.5 text-[0.75rem] text-[var(--color-danger)]"
              >
                {attachmentError}
              </span>
            ) : null}
          </div>
        ) : null}

        {uploadSizeWarning ? (
          <div
            role="status"
            className="mx-[1.05rem] mb-2 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)]"
          >
            {uploadSizeWarning}
          </div>
        ) : null}

        {spreadsheetNudge.show ? (
          <div
            role="status"
            className="mx-[1.05rem] mb-2 flex flex-wrap items-center justify-between gap-2 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)]"
          >
            <span>
              Spreadsheets analyze better on{" "}
              <span className="font-medium text-[var(--color-text-primary)]">
                {currentAdvancedModelLabel()}
              </span>
              .
            </span>
            <span className="flex items-center gap-2">
              <button
                type="button"
                className="rounded-[var(--radius-md)] border border-[var(--color-accent)] px-2.5 py-0.5 text-[0.7rem] text-[var(--color-text-primary)] transition hover:bg-[var(--color-accent)] hover:text-[var(--color-surface-1)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40"
                disabled={isStreaming || shareBusy}
                onClick={() => {
                  if (shareBusy) return;
                  setSelectedModel(spreadsheetNudge.recommendedModel);
                  setSpreadsheetNudgeDismissed(true);
                }}
              >
                Switch
              </button>
              <button
                type="button"
                aria-label="Dismiss model suggestion"
                className="text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
                onClick={() => setSpreadsheetNudgeDismissed(true)}
              >
                <Icon name="close" className="size-3" />
              </button>
            </span>
          </div>
        ) : null}

        <input
          ref={fileInputRef}
          type="file"
          multiple
          disabled={isStreaming || isUploadingAttachments || shareBusy}
          className="hidden"
          onChange={(event) => {
            if (shareBusy) {
              event.target.value = "";
              return;
            }
            addAttachmentFiles(event.target.files);
            // Reset so picking the same file twice in a row still fires onChange.
            event.target.value = "";
          }}
        />

        <div className="flex items-center justify-between gap-2 px-[0.7rem] pt-[0.45rem] pb-[0.6rem]">
          {/* One row, always. The icon buttons are fixed-width and never
                    shrink; the model chip is the single elastic item, so a
                    narrow (phone) toolbar squeezes the model label instead of
                    wrapping the trailing controls onto a second line. */}
          <div className="flex min-w-0 flex-1 items-center gap-[0.15rem] overflow-visible sm:gap-[0.35rem]">
            {/* Model chip (the design's .model-chip): icon + current
                      model + caret opening the .composer-pop listbox. The
                      search field at the top of the popover preserves the
                      old inline input's type-to-search-the-catalog and free
                      slug entry; Esc closes and returns focus to the chip. */}
            {/* The wrapper is a CSS positioning anchor and nothing else —
                      role="presentation" states that rather than inventing a
                      widget role for it. Escape is delegated here on purpose:
                      the trigger button, the search field and the listbox rows
                      are all real controls inside it, and a composite widget's
                      Escape has to work from whichever one holds focus. */}
            <div
              ref={modelPickerRef}
              role="presentation"
              className="relative inline-flex min-w-0"
              onKeyDown={(event) => {
                if (event.key === "Escape" && modelPickerOpen) {
                  event.stopPropagation();
                  closeModelPicker(true);
                }
              }}
            >
              <button
                ref={modelChipRef}
                type="button"
                aria-haspopup="listbox"
                aria-expanded={modelPickerOpen}
                disabled={isStreaming || shareBusy}
                title={
                  modelError
                    ? modelError.message
                    : `OpenRouter model slug — e.g. ${currentDefaultModel()} (recommended) or ${currentAdvancedModel()} (strongest)`
                }
                className={`inline-flex h-[1.95rem] min-w-0 items-center gap-[0.25rem] rounded-[var(--radius-md)] py-[0.3rem] pl-[0.4rem] pr-[0.3rem] text-[0.78rem] font-medium transition sm:gap-[0.4rem] sm:pl-[0.6rem] sm:pr-[0.5rem] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40 ${
                  modelError
                    ? "text-[var(--color-danger)]"
                    : modelPickerOpen
                      ? "bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] text-[var(--color-text-primary)]"
                      : "text-[var(--color-text-secondary)] hover:bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] hover:text-[var(--color-text-primary)]"
                }`}
                onClick={() => {
                  if (modelPickerOpen) closeModelPicker(false);
                  else openModelPicker();
                }}
              >
                <Icon
                  name="model"
                  className={`size-[0.85rem] shrink-0 ${modelError ? "" : "text-[var(--color-accent)]"}`}
                />
                {/* Two renderings of the same label, one visible at a
                          time: phones/small tablets get the vendor prefix
                          dropped ("Z.AI: GLM 5.2" → "GLM 5.2") so the model
                          is still readable in the space left after the icon
                          buttons; ≥sm shows the full catalog name. Both
                          truncate with an ellipsis rather than pushing the
                          toolbar wider. aria-hidden on the short one so the
                          accessible name stays the full label exactly once. */}
                <span
                  aria-hidden="true"
                  data-testid="composer-model-label-short"
                  className="truncate sm:hidden"
                >
                  {compactModelLabel(selectedModelLabel)}
                </span>
                <span
                  data-testid="composer-model-label-full"
                  className="hidden max-w-[11rem] truncate sm:inline"
                >
                  {selectedModelLabel}
                </span>
                <span className="sr-only sm:hidden">{selectedModelLabel}</span>
                {/* Cost tier for the *selected* model, so the running
                          price band is visible without opening the picker.
                          Hidden below sm — four glyphs are the widest thing
                          on the chip that isn't the model's name, and the
                          tier is still shown on every row of the picker.
                          The wrapper carries the responsive display because
                          .model-cost sets `display: inline-flex` outside
                          Tailwind's utility layer and would win otherwise. */}
                <span className="hidden shrink-0 items-center sm:inline-flex">
                  <ModelCostIndicator
                    prices={selectedModelPrices}
                    free={selectedModelFree}
                  />
                </span>
                <Icon
                  name="selector"
                  className="size-[0.8rem] shrink-0 text-[var(--color-text-muted)]"
                />
              </button>
              {modelPickerOpen && !isStreaming && !shareBusy ? (
                <div className={COMPOSER_POP_WIDE}>
                  <input
                    ref={modelInputRef}
                    type="text"
                    spellCheck={false}
                    autoCapitalize="off"
                    autoCorrect="off"
                    placeholder="Search models…"
                    aria-label="Model"
                    role="combobox"
                    aria-expanded="true"
                    aria-controls="composer-model-listbox"
                    aria-activedescendant={
                      filteredRankedModels.length > 0
                        ? `composer-model-opt-${Math.min(modelHighlight, filteredRankedModels.length - 1)}`
                        : undefined
                    }
                    className="w-full rounded-[0.5rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.6rem] py-[0.32rem] text-[0.82rem] text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-border-strong)]"
                    value={modelSearchQuery}
                    onChange={(event) => {
                      // Typing only filters the catalog. The text is a
                      // draft, NOT the selected model: it used to be
                      // written straight into selectedModel on every
                      // keystroke, so typing "cla" and then pressing
                      // Escape (or clicking away — both only close the
                      // popover) left "cla" as the model and the next
                      // send failed. selectedModel changes only on a
                      // commit (Enter or a row pick, below); a dismiss
                      // leaves whatever was selected before untouched.
                      setModelSearchQuery(event.target.value);
                      setModelHighlight(0);
                    }}
                    onKeyDown={(event) => {
                      const count = filteredRankedModels.length;
                      if (event.key === "ArrowDown" && count > 0) {
                        event.preventDefault();
                        setModelHighlight(
                          (h) => (Math.min(h, count - 1) + 1) % count,
                        );
                      } else if (event.key === "ArrowUp" && count > 0) {
                        event.preventDefault();
                        setModelHighlight(
                          (h) => (Math.min(h, count - 1) - 1 + count) % count,
                        );
                      } else if (event.key === "Enter") {
                        event.preventDefault();
                        // Commit: the highlighted row when there is one;
                        // otherwise the typed text itself, which keeps
                        // free slug entry working (a slug the catalog
                        // doesn't list has no row to pick). Empty text
                        // commits nothing and just closes.
                        const pick =
                          filteredRankedModels[
                            Math.min(modelHighlight, count - 1)
                          ];
                        const typed = modelSearchQuery.trim();
                        if (pick) setSelectedModel(pick.slug);
                        else if (typed) setSelectedModel(typed);
                        closeModelPicker(true);
                      }
                    }}
                  />
                  <div
                    id="composer-model-listbox"
                    role="listbox"
                    aria-label="Model options"
                    className="grid max-h-72 grid-cols-[minmax(0,1fr)] gap-[0.1rem] overflow-y-auto"
                  >
                    {isLoadingRankedModels ||
                    (isLoadingCatalog && modelSearchQuery.trim() !== "") ? (
                      <div className="px-[0.6rem] py-[0.45rem] text-[0.74rem] text-[var(--color-text-muted)]">
                        Loading...
                      </div>
                    ) : filteredRankedModels.length === 0 ? (
                      <div className="px-[0.6rem] py-[0.45rem] text-[0.74rem] text-[var(--color-text-muted)]">
                        No matches
                      </div>
                    ) : (
                      filteredRankedModels.map((model, i) => {
                        // One pill per row, picked from a strict
                        // hierarchy so the listbox stays uncluttered:
                        //   recommended > workspace > tested > ✨ new >
                        //   experimental. recommended = one of the two
                        //   pinned slugs, styled as the design's
                        //   .rec-tag; workspace = a slug served by an
                        //   admin-configured provider.
                        const tier = model.slug
                          ? tierForModel(model.slug)
                          : null;
                        const isTier =
                          tier === "default" || tier === "advanced";
                        const isFresh = isNewlyReleased(model.created);
                        // An operator pin (FLEET_PINNED_MODELS) ranks right
                        // under recommended; its tag ("alpha", …) is the
                        // pill, and a relabelled pin's tooltip omits the
                        // underlying slug it renames.
                        let pill: ReactNode = null;
                        if (isTier) {
                          // Same size/case as the tested/experimental
                          // badges — green is the only differentiator.
                          pill = (
                            <StatusChip tone="success" className="shrink-0 tabular-nums">
                              recommended
                            </StatusChip>
                          );
                        } else if (model.pinTag) {
                          pill = (
                            <StatusChip tone="warning" testId="model-pin-tag" className="shrink-0 tabular-nums">
                              {model.pinTag}
                            </StatusChip>
                          );
                        } else if (model.workspace) {
                          pill = (
                            <StatusChip tone="accent" className="shrink-0 tabular-nums">
                              workspace
                            </StatusChip>
                          );
                        } else if (tier === "tested") {
                          pill = <ModelValidationBadge tier="tested" />;
                        } else if (isFresh) {
                          pill = <NewModelBadge />;
                        } else if (tier === "experimental") {
                          pill = <ModelValidationBadge tier="experimental" />;
                        }
                        const isSelected = model.slug === selectedModel;
                        const isHighlighted =
                          i ===
                          Math.min(
                            modelHighlight,
                            filteredRankedModels.length - 1,
                          );
                        return (
                          <button
                            key={model.slug || "__default__"}
                            id={`composer-model-opt-${i}`}
                            type="button"
                            role="option"
                            aria-selected={isSelected}
                            title={
                              !model.slug
                                ? "Use the server-configured default model"
                                : model.relabelled
                                  ? model.name
                                  : `${model.name} (${model.slug})`
                            }
                            className={`${POP_ROW} ${isSelected ? POP_ROW_SELECTED : ""} ${
                              isHighlighted && !isSelected
                                ? "bg-[var(--rail-hover)]"
                                : ""
                            }`}
                            onClick={() => {
                              setSelectedModel(model.slug);
                              closeModelPicker(true);
                            }}
                          >
                            <span
                              className={`${POP_TITLE} min-w-0 break-words`}
                            >
                              {model.name}
                            </span>
                            {/* Right-side meta cluster: the single
                                      status pill (if any) plus the cost tier.
                                      Grouped so the row's justify-between
                                      keeps them together at the trailing
                                      edge instead of spreading them apart. */}
                            <span className="flex shrink-0 items-center gap-[0.35rem]">
                              {pill}
                              <ModelCostIndicator
                                free={model.free}
                                prices={{
                                  pricePrompt: model.pricePrompt,
                                  priceCompletion: model.priceCompletion,
                                }}
                              />
                            </span>
                          </button>
                        );
                      })
                    )}
                  </div>
                </div>
              ) : null}
            </div>
            {/* The design's .composer-div divider between the model
                      chip and the icon-button cluster. */}
            <span
              aria-hidden="true"
              className="mx-[0.05rem] h-[1.1rem] w-px shrink-0 bg-[var(--color-border-strong)] sm:mx-[0.15rem]"
            />
            <button
              type="button"
              aria-label="Attach files"
              data-tip-top="Attach files"
              className={TOOL_BTN}
              disabled={isStreaming || isUploadingAttachments || shareBusy}
              onClick={() => fileInputRef.current?.click()}
            >
              <Icon name="paperclip" className="size-3.5" />
            </button>
            {mcpServers.length > 0 ? (
              // Positioning anchor only (see the model picker above):
              // presentational wrapper, Escape delegated so it works from
              // the trigger button or from any row of the open popover. The
              // popover's search field handles its own Escape (clear first,
              // then close) and stops it here.
              <div
                ref={mcpPickerRef}
                role="presentation"
                className="relative inline-flex"
                onKeyDown={(event) => {
                  if (event.key === "Escape" && mcpPickerOpen) {
                    event.stopPropagation();
                    setMcpPickerOpen(false);
                    mcpButtonRef.current?.focus();
                  }
                }}
              >
                <button
                  ref={mcpButtonRef}
                  type="button"
                  aria-label="Tools"
                  aria-haspopup="true"
                  aria-expanded={mcpPickerOpen}
                  disabled={isStreaming || shareBusy}
                  // No tooltip while the menu is open: it would sit over the
                  // menu's footer, and a tap leaves it showing on a phone.
                  data-tip-top={mcpPickerOpen ? undefined : `Tools, ${toolsBadge} on`}
                  className={`${TOOL_BTN} ${mcpPickerOpen ? TOOLS_BTN_OPEN : ""}`}
                  onClick={() => {
                    const next = !mcpPickerOpen;
                    if (next) setHeldToolsOn(liveToolsOn);
                    setMcpPickerOpen(next);
                    // Pre-chat: the preview catalog loaded at
                    // startup is already in state — no per-conv
                    // row to fetch yet.
                    if (next && activeConversationId) {
                      void loadMcpServerCatalog(activeConversationId);
                    }
                  }}
                >
                  <Icon name="tools" className="size-[1.0625rem]" />
                  {/* The design's .tool-badge: how many connections are on
                      in this chat, always-on included, pinned to the
                      button's top-right corner. */}
                  {toolsBadge > 0 ? (
                    <span
                      data-testid="chat-tools-badge"
                      className="pointer-events-none absolute -right-[5px] -top-[7px] inline-flex h-[15px] min-w-[15px] items-center justify-center rounded-[var(--radius-pill)] bg-[var(--color-accent-strong)] px-[3px] text-[0.6rem] font-semibold tabular-nums text-[var(--color-on-accent-strong)]"
                    >
                      {toolsBadge}
                    </span>
                  ) : null}
                </button>
                {mcpPickerOpen && !isStreaming && !shareBusy ? (
                  <ToolsPopover
                    servers={mcpServers}
                    categories={pickerCategories}
                    appName={appName}
                    isAdmin={isAdmin}
                    loading={isLoadingMcpServers}
                    onToggle={(name) => void toggleMcpServer(activeConversationId, name)}
                    onSetEnabled={(names, enabled) =>
                      void setMcpServersEnabled(activeConversationId, names, enabled)
                    }
                    onSetAccount={(name, account) =>
                      void setMcpServerAccount(activeConversationId, name, account)
                    }
                    onClose={() => {
                      setMcpPickerOpen(false);
                      mcpButtonRef.current?.focus();
                    }}
                    onOpenLibrary={(name) => {
                      setMcpPickerOpen(false);
                      setToolsLibrary({ selected: name });
                    }}
                  />
                ) : null}
                {toolsLibrary && !shareBusy ? (
                  <ToolsLibrary
                    servers={mcpServers}
                    categories={pickerCategories}
                    appName={appName}
                    isAdmin={isAdmin}
                    initialSelected={toolsLibrary.selected}
                    onClose={() => {
                      setToolsLibrary(null);
                      mcpButtonRef.current?.focus();
                    }}
                    onToggle={(name) => void toggleMcpServer(activeConversationId, name)}
                    onSetEnabled={(names, enabled) =>
                      void setMcpServersEnabled(activeConversationId, names, enabled)
                    }
                    onSetAccount={(name, account) =>
                      void setMcpServerAccount(activeConversationId, name, account)
                    }
                  />
                ) : null}
              </div>
            ) : null}
            {/* Keep PromptLibrary mounted at the toolbar root so its portalled
                dialog stays open after the overflow menu closes. */}
            {!shareBusy ? (
              <PromptLibrary
                currentText={prompt}
                onInsert={(next) => {
                  if (!shareBusy) setPrompt(next);
                }}
                compact
                returnFocusRef={moreButtonRef}
                renderTrigger={(openLibrary) => {
                  openPromptLibraryRef.current = openLibrary;
                  return null;
                }}
              />
            ) : null}
            {(() => {
              const hasStartedChatting = messages.length > 0;
              const personaLocked =
                isStreaming ||
                hasStartedChatting ||
                isConversationOwner === false;
              const workspaceOptions =
                personas.length > 0
                  ? personas
                  : isUserPersonaRef(selectedPersona)
                    ? []
                    : [selectedPersona];
              const currentPersonaLabel = personaLabel(selectedPersona, userPersonas);
              const effectiveDefaultPersona = defaultPersona || personas[0] || "";
              const isNonDefaultPersona =
                isUserPersonaRef(selectedPersona) ||
                (effectiveDefaultPersona !== "" &&
                  selectedPersona !== effectiveDefaultPersona);
              const canCompact =
                Boolean(activeConversationId) &&
                messages.length >= 2 &&
                isConversationOwner !== false;
              const usagePct =
                contextUsage?.fraction != null
                  ? Math.min(999, Math.round(contextUsage.fraction * 100))
                  : null;
              const isMultiplayer = Boolean(shareConversation?.multiplayer);
              const isTeamShared = Boolean(
                shareConversation?.team_visible && !shareConversation?.multiplayer,
              );
              const isLinkShared = Boolean(
                shareConversation?.share_token &&
                  !shareConversation?.team_visible &&
                  !shareConversation?.multiplayer,
              );
              const isSharedAny =
                Boolean(onOpenShare) &&
                (isMultiplayer || isTeamShared || isLinkShared);
              // A public link can sit beside team or multiplayer sharing; it
              // is never hidden behind them (anyone with the URL can read).
              const alsoLink = Boolean(shareConversation?.share_token) && !isLinkShared;
              const linkSuffix = alsoLink ? " and by public link" : "";
              const shareAriaLabel = isMultiplayer
                ? `Multiplayer${linkSuffix} — open sharing`
                : isTeamShared
                  ? `Shared with your team${linkSuffix} — open sharing`
                  : isLinkShared
                    ? "Public link — open sharing"
                    : "Share this chat";
              const shareTipText = isMultiplayer
                ? `Multiplayer chat is on${alsoLink ? ", plus a public link" : ""} — click to manage participants or sharing`
                : isTeamShared
                  ? `Shared with your team${linkSuffix} — click to manage sharing`
                  : isLinkShared
                    ? "Public link active — click to manage sharing"
                    : "Share this chat or start multiplayer";
              const shareDisabled =
                shareBusy ||
                isStreaming ||
                isUploadingAttachments ||
                ((activeConversationId === null ||
                  isPendingKey(activeConversationId)) &&
                  messages.length > 0);
              const triggerShare = () => {
                if (moreMenuOpen) {
                  setMoreMenuOpen(false);
                  moreButtonRef.current?.focus();
                }
                setMcpPickerOpen(false);
                setToolsLibrary(null);
                setPersonaPickerOpen(false);
                setModelPickerOpen(false);
                setPatrolPopoverOpen(false);
                onOpenShare?.();
              };
              const hasActiveSecondary =
                isAutoApprove ||
                activePatrolCount > 0 ||
                (!personaLocked && isNonDefaultPersona) ||
                isSharedAny;
              return (
                <>
                  {/* Overflow menu ("More options"): consolidates Prompt library,
                      Persona, Patrol schedule, Share, Auto-approve tools, Compact,
                      and Send on Enter into a clean labeled popover so the primary
                      composer bar stays calm on both mobile and desktop. */}
                  <div
                    ref={moreMenuRef}
                    role="presentation"
                    className="relative inline-flex"
                    onKeyDown={(event) => {
                      if (!moreMenuOpen) return;
                      if (event.key === "Escape") {
                        event.stopPropagation();
                        setMoreMenuOpen(false);
                        moreButtonRef.current?.focus();
                        return;
                      }
                      if (
                        event.key === "ArrowDown" ||
                        event.key === "ArrowUp" ||
                        event.key === "Home" ||
                        event.key === "End"
                      ) {
                        const items = Array.from(
                          moreMenuRef.current?.querySelectorAll<HTMLElement>(
                            '[role="menu"] [role^="menuitem"]:not([disabled])',
                          ) ?? [],
                        );
                        if (items.length === 0) return;
                        event.preventDefault();
                        const currentIndex = items.indexOf(
                          document.activeElement as HTMLElement,
                        );
                        let nextIndex = 0;
                        if (event.key === "Home") {
                          nextIndex = 0;
                        } else if (event.key === "End") {
                          nextIndex = items.length - 1;
                        } else if (event.key === "ArrowDown") {
                          nextIndex =
                            currentIndex >= 0
                              ? (currentIndex + 1) % items.length
                              : 0;
                        } else {
                          nextIndex =
                            currentIndex > 0
                              ? currentIndex - 1
                              : items.length - 1;
                        }
                        items[nextIndex]?.focus();
                      }
                    }}
                  >
                    <button
                      ref={moreButtonRef}
                      type="button"
                      data-testid="composer-more-button"
                      aria-label="More options"
                      aria-haspopup="true"
                      aria-expanded={moreMenuOpen}
                      disabled={shareBusy}
                      data-tip-top={moreMenuOpen ? undefined : "Prompts, persona, schedule & more"}
                      className={`${TOOL_BTN} ${
                        moreMenuOpen
                          ? TOOLS_BTN_OPEN
                          : hasActiveSecondary
                            ? TOOL_BTN_ACTIVE
                            : ""
                      }`}
                      onClick={() => setMoreMenuOpen((v) => !v)}
                    >
                      <Icon name="dots" className="size-4 rotate-90" />
                      {hasActiveSecondary && !moreMenuOpen ? (
                        <span
                          aria-hidden="true"
                          className="pointer-events-none absolute right-1 top-1 size-1.5 rounded-full bg-[var(--color-accent)]"
                        />
                      ) : null}
                    </button>
                    {moreMenuOpen && !shareBusy ? (
                      <div
                        data-testid="composer-more-popover"
                        role="menu"
                        aria-label="More composer options"
                        className={`${COMPOSER_POP_WIDE} max-h-[min(75vh,30rem)] overflow-y-auto sm:w-[20.5rem]`}
                      >
                        <button
                          type="button"
                          role="menuitem"
                          aria-label="Open prompt library"
                          className={POP_ROW}
                          onClick={() => {
                            setMoreMenuOpen(false);
                            moreButtonRef.current?.focus();
                            openPromptLibraryRef.current?.();
                          }}
                        >
                          <span className="flex min-w-0 items-center gap-2.5">
                            <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                              <Icon name="book" className="size-3.5" />
                            </span>
                            <span className="grid min-w-0">
                              <span className={POP_TITLE}>Prompt library</span>
                              <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                Browse and insert reusable prompts
                              </span>
                            </span>
                          </span>
                          <Icon name="chevron-right" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
                        </button>

                        <button
                          type="button"
                          role="menuitem"
                          disabled={personaLocked}
                          title={
                            personaLocked
                              ? "Personas can only be changed in a new chat"
                              : undefined
                          }
                          aria-label={
                            personaLocked
                              ? `Persona — ${currentPersonaLabel} (personas can only be changed in a new chat)`
                              : `Persona — ${currentPersonaLabel}`
                          }
                          className={`${POP_ROW} disabled:cursor-not-allowed disabled:opacity-45`}
                          onClick={() => {
                            if (personaLocked) return;
                            setMoreMenuOpen(false);
                            setPersonaPickerOpen(true);
                          }}
                        >
                          <span className="flex min-w-0 items-center gap-2.5">
                            <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                              <Icon name="persona" className="size-3.5" />
                            </span>
                            <span className="grid min-w-0">
                              <span className={POP_TITLE}>Persona</span>
                              <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                {personaLocked
                                  ? "Personas can only be changed in a new chat"
                                  : "Voice and instructions for this chat"}
                              </span>
                            </span>
                          </span>
                          <span className="inline-flex shrink-0 items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-secondary)]">
                            <span className="max-w-[6.5rem] truncate">{currentPersonaLabel}</span>
                            {!personaLocked ? (
                              <Icon name="chevron-right" className="size-3" />
                            ) : null}
                          </span>
                        </button>

                        {onOpenMemories ? (
                          <button
                            type="button"
                            role="menuitem"
                            data-testid="composer-memories-button"
                            aria-label="Manage memories"
                            className={POP_ROW}
                            onClick={() => {
                              setMoreMenuOpen(false);
                              moreButtonRef.current?.focus();
                              onOpenMemories();
                            }}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                                <Icon name="brain" className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Memories</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  Saved facts and team learnings
                                </span>
                              </span>
                            </span>
                            <Icon name="chevron-right" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
                          </button>
                        ) : null}

                        {onOpenShare || (onStartPatrol && !sealed) || toggleAutoApprove || canCompact ? (
                          <div aria-hidden="true" className="my-0.5 h-px bg-[var(--color-border)]" />
                        ) : null}

                        {onOpenShare ? (
                          <button
                            type="button"
                            role="menuitem"
                            data-testid="composer-share-button"
                            disabled={shareDisabled}
                            aria-label={shareAriaLabel}
                            className={`${POP_ROW} disabled:opacity-40`}
                            onClick={triggerShare}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span
                                className={`inline-flex size-7 shrink-0 items-center justify-center rounded-md ${
                                  isSharedAny
                                    ? "bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]"
                                    : "bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]"
                                }`}
                              >
                                {isMultiplayer || isTeamShared ? (
                                  <TeamGlyph className="size-3.5 shrink-0" />
                                ) : isLinkShared ? (
                                  <ShareGlyph className="size-3.5 shrink-0" />
                                ) : (
                                  <Icon name="external-link" className="size-3.5 shrink-0" />
                                )}
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Share this chat</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  {isMultiplayer
                                    ? `Multiplayer active${alsoLink ? " · public link on" : ""}`
                                    : isTeamShared
                                      ? `Shared with your team${alsoLink ? " · public link on" : ""}`
                                      : isLinkShared
                                        ? "Public link active — anyone with the link can read"
                                        : "Share with your team, by link, or start multiplayer"}
                                </span>
                              </span>
                            </span>
                            <span
                              className={`inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 text-[0.68rem] font-semibold ${
                                isSharedAny
                                  ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                                  : "border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-muted)]"
                              }`}
                            >
                              {isMultiplayer ? (
                                "Multiplayer"
                              ) : isTeamShared ? (
                                "Shared"
                              ) : isLinkShared ? (
                                "Link on"
                              ) : (
                                <>
                                  <span>Only you</span>
                                  <Icon name="chevron-right" className="size-3" />
                                </>
                              )}
                            </span>
                          </button>
                        ) : null}

                        {onStartPatrol && !sealed ? (
                          <button
                            type="button"
                            role="menuitem"
                            data-testid="chat-patrol-button"
                            aria-label="Patrol & recurring schedule"
                            className={POP_ROW}
                            onClick={() => {
                              setMoreMenuOpen(false);
                              moreButtonRef.current?.focus();
                              setPatrolPopoverOpen(true);
                            }}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                                <Icon name="clock" className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Patrol & schedule</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  {activePatrolCount > 0
                                    ? `${activePatrolCount} recurring schedule active`
                                    : "Run this chat on a recurring schedule"}
                                </span>
                              </span>
                            </span>
                            <span className="inline-flex shrink-0 items-center gap-1 text-[0.7rem] font-medium text-[var(--color-text-muted)]">
                              {activePatrolCount > 0 ? `${activePatrolCount} active` : "Set up"}
                              <Icon name="chevron-right" className="size-3.5" />
                            </span>
                          </button>
                        ) : null}

                        {toggleAutoApprove ? (
                          <button
                            type="button"
                            role="menuitemcheckbox"
                            aria-checked={isAutoApprove}
                            aria-label={
                              isAutoApprove
                                ? "Auto-approve tools in this chat is on (click to turn off)"
                                : "Auto-approve tools in this chat (click to turn on)"
                            }
                            className={POP_ROW}
                            onClick={() => toggleAutoApprove()}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span
                                className={`inline-flex size-7 shrink-0 items-center justify-center rounded-md ${
                                  isAutoApprove
                                    ? "bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]"
                                    : "bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]"
                                }`}
                              >
                                <Icon name="zap" className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Auto-approve tools</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  Skip approval cards for routine actions
                                </span>
                              </span>
                            </span>
                            <span
                              className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[0.68rem] font-semibold ${
                                isAutoApprove
                                  ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                                  : "border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-muted)]"
                              }`}
                            >
                              {isAutoApprove ? "On" : "Off"}
                            </span>
                          </button>
                        ) : null}

                        {canCompact ? (
                          <button
                            type="button"
                            role="menuitem"
                            disabled={isStreaming || isSummarizing}
                            aria-label={
                              isSummarizing
                                ? "Compacting conversation…"
                                : usagePct !== null
                                  ? `Compact conversation (${usagePct}% full)`
                                  : "Compact this conversation"
                            }
                            className={`${POP_ROW} disabled:opacity-40`}
                            onClick={() => {
                              setMoreMenuOpen(false);
                              moreButtonRef.current?.focus();
                              setConfirmSummarize(true);
                            }}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                                <Icon name="compress" className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Compact conversation</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  {isSummarizing
                                    ? "Compacting earlier turns…"
                                    : usagePct !== null
                                      ? `${usagePct}% of context window used`
                                      : "Summarize earlier turns to save context"}
                                </span>
                              </span>
                            </span>
                            <Icon name="chevron-right" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
                          </button>
                        ) : null}

                        <div aria-hidden="true" className="my-0.5 h-px bg-[var(--color-border)]" />

                        {onToggleShowDetails ? (
                          <button
                            type="button"
                            role="menuitemcheckbox"
                            aria-checked={showDetails}
                            data-testid="composer-details-toggle"
                            aria-label={
                              showDetails
                                ? "Hide details (thinking, stats, tool calls)"
                                : "Show details (thinking, stats, tool calls)"
                            }
                            className={POP_ROW}
                            onClick={() => onToggleShowDetails()}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span
                                className={`inline-flex size-7 shrink-0 items-center justify-center rounded-md ${
                                  showDetails
                                    ? "bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]"
                                    : "bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]"
                                }`}
                              >
                                <Icon name={showDetails ? "info" : "sparkles"} className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Show details</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  Thinking, tool calls, and turn stats
                                </span>
                              </span>
                            </span>
                            <span
                              className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[0.68rem] font-semibold ${
                                showDetails
                                  ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                                  : "border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-muted)]"
                              }`}
                            >
                              {showDetails ? "On" : "Off"}
                            </span>
                          </button>
                        ) : null}

                        {onToggleDashboardView ? (
                          <button
                            type="button"
                            role="menuitemcheckbox"
                            aria-checked={dashboardView}
                            data-testid="composer-dashboard-toggle"
                            className={POP_ROW}
                            onClick={() => {
                              setMoreMenuOpen(false);
                              onToggleDashboardView();
                            }}
                          >
                            <span className="flex min-w-0 items-center gap-2.5">
                              <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                                <Icon name="grid" className="size-3.5" />
                              </span>
                              <span className="grid min-w-0">
                                <span className={POP_TITLE}>Dashboard view</span>
                                <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                  Live panels beside the chat · press D
                                </span>
                              </span>
                            </span>
                            <span
                              className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[0.68rem] font-semibold ${
                                dashboardView
                                  ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                                  : "border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-muted)]"
                              }`}
                            >
                              {dashboardView ? "On" : "Off"}
                            </span>
                          </button>
                        ) : null}

                        <button
                          type="button"
                          role="menuitemcheckbox"
                          aria-checked={sendOnEnter}
                          aria-label={
                            sendOnEnter
                              ? "Send on Enter (click to switch to Ctrl+Enter)"
                              : "Send on Ctrl+Enter (click to switch to Enter)"
                          }
                          className={POP_ROW}
                          onClick={() => setSendOnEnter((v) => !v)}
                        >
                          <span className="flex min-w-0 items-center gap-2.5">
                            <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]">
                              <Icon name="return-key" className="size-3.5" />
                            </span>
                            <span className="grid min-w-0">
                              <span className={POP_TITLE}>Send on Enter</span>
                              <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                {sendOnEnter
                                  ? "Enter sends · Shift+Enter adds a line"
                                  : "Ctrl/⌘+Enter sends · Enter adds a line"}
                              </span>
                            </span>
                          </span>
                          <span
                            className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 text-[0.68rem] font-semibold ${
                              sendOnEnter
                                ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                                : "border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-muted)]"
                            }`}
                          >
                            {sendOnEnter ? "On" : "Off"}
                          </span>
                        </button>
                      </div>
                    ) : null}
                  </div>

                  {/* Contextual Persona chip + picker: rendered on the bar when
                      a non-default persona is active or while the picker is open.
                      Passive status chip is hidden on phone viewports (< sm) so
                      active secondary indicators never crowd Share/Send. */}
                  {!personaLocked && (isNonDefaultPersona || personaPickerOpen) ? (
                    <div
                      ref={personaPickerRef}
                      role="presentation"
                      className={`relative ${
                        personaPickerOpen ? "inline-flex" : "hidden sm:inline-flex"
                      }`}
                      onKeyDown={(event) => {
                        if (event.key === "Escape" && personaPickerOpen) {
                          event.stopPropagation();
                          setPersonaPickerOpen(false);
                          (isNonDefaultPersona
                            ? personaButtonRef.current
                            : moreButtonRef.current
                          )?.focus();
                        }
                      }}
                    >
                      <button
                        ref={personaButtonRef}
                        type="button"
                        aria-haspopup="listbox"
                        aria-expanded={personaPickerOpen}
                        disabled={isStreaming || shareBusy}
                        aria-label={`Persona — ${currentPersonaLabel}`}
                        data-tip-top={`Persona — ${currentPersonaLabel}`}
                        className={`${TOOL_BTN} ${TOOL_BTN_ACTIVE}`}
                        onClick={() => setPersonaPickerOpen((open) => !open)}
                      >
                        <Icon name="persona" className="size-3.5" />
                      </button>
                      {personaPickerOpen && !shareBusy ? (
                        <div className={`${COMPOSER_POP_WIDE} max-h-[min(26rem,70vh)] overflow-y-auto`}>
                          <div role="listbox" aria-label="Persona" className="grid gap-[0.1rem]">
                            {(() => {
                              const pick = (p: string) => {
                                const nextIsNonDefault =
                                  isUserPersonaRef(p) ||
                                  (effectiveDefaultPersona !== "" &&
                                    p !== effectiveDefaultPersona);
                                setSelectedPersona(p);
                                setPersonaPickerOpen(false);
                                (nextIsNonDefault
                                  ? personaButtonRef.current
                                  : moreButtonRef.current
                                )?.focus();
                              };
                              const option = (value: string, title: string, sub?: string) => {
                                const selected = value === selectedPersona;
                                return (
                                  <button
                                    key={value}
                                    type="button"
                                    role="option"
                                    aria-selected={selected}
                                    className={`${POP_ROW} ${selected ? POP_ROW_SELECTED : ""}`}
                                    onClick={() => pick(value)}
                                  >
                                    <span className="grid min-w-0">
                                      <span className={`${POP_TITLE} truncate`}>{title}</span>
                                      {sub ? (
                                        <span className="truncate text-[0.7rem] text-[var(--color-text-muted)]">
                                          {sub}
                                        </span>
                                      ) : null}
                                    </span>
                                    {selected ? (
                                      <span
                                        aria-hidden="true"
                                        className="size-1.5 shrink-0 rounded-full bg-[var(--color-accent)]"
                                      />
                                    ) : null}
                                  </button>
                                );
                              };
                              const groupHead =
                                "px-[0.6rem] pb-[0.15rem] pt-[0.35rem] text-[0.66rem] font-semibold uppercase tracking-wide text-[var(--color-text-muted)]";
                              return userPersonas.length > 0 ? (
                                <>
                                  <div role="group" aria-label="Your personas" className="grid gap-[0.1rem]">
                                    <div aria-hidden="true" className={groupHead}>
                                      Yours
                                    </div>
                                    {userPersonas.map((p) =>
                                      option(
                                        p.ref,
                                        p.display_name || p.name,
                                        [
                                          p.kind === "twin" ? "Digital twin" : null,
                                          p.autonomy === "act" ? "acts for you" : "suggests actions",
                                        ]
                                          .filter(Boolean)
                                          .join(" · "),
                                      ),
                                    )}
                                  </div>
                                  <div role="group" aria-label="Workspace personas" className="grid gap-[0.1rem]">
                                    <div aria-hidden="true" className={groupHead}>
                                      Workspace
                                    </div>
                                    {workspaceOptions.map((p) => option(p, formatBundlePersona(p)))}
                                  </div>
                                </>
                              ) : (
                                workspaceOptions.map((p) => option(p, formatBundlePersona(p)))
                              );
                            })()}
                          </div>
                          <Link
                            href="/settings/personas"
                            className="mt-[0.2rem] flex items-center gap-[0.4rem] rounded-[0.5rem] border-t border-[var(--color-border-subtle)] px-[0.6rem] py-[0.45rem] text-[0.76rem] text-[var(--color-text-secondary)] no-underline transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                          >
                            <Icon name="plus" className="size-3" />
                            {userPersonas.length > 0 ? "Manage your personas" : "Create your own persona"}
                          </Link>
                        </div>
                      ) : null}
                    </div>
                  ) : null}

                  {/* Contextual Patrol indicator + popover: rendered on the bar when
                      at least one patrol schedule is active or while the popover is open. */}
                  {onStartPatrol && !sealed && (activePatrolCount > 0 || patrolPopoverOpen) ? (
                    <div
                      ref={patrolPopoverRef}
                      role="presentation"
                      className={`relative ${
                        patrolPopoverOpen ? "inline-flex" : "hidden sm:inline-flex"
                      }`}
                    >
                      <button
                        ref={patrolButtonRef}
                        type="button"
                        aria-label="Patrol & recurring schedule"
                        aria-haspopup="true"
                        aria-expanded={patrolPopoverOpen}
                        disabled={shareBusy}
                        data-testid="chat-patrol-button"
                        data-tip-top={
                          patrolPopoverOpen
                            ? undefined
                            : activePatrolCount > 0
                              ? `Patrols (${activePatrolCount} active)`
                              : "Start recurring patrol or timer"
                        }
                        className={`${TOOL_BTN} ${
                          patrolPopoverOpen ? TOOLS_BTN_OPEN : TOOL_BTN_ACTIVE
                        }`}
                        onClick={() => setPatrolPopoverOpen((v) => !v)}
                      >
                        <Icon name="clock" className="size-3.5" />
                        {activePatrolCount > 0 ? (
                          <span
                            data-testid="chat-patrol-badge"
                            className="pointer-events-none absolute -right-[5px] -top-[7px] inline-flex h-[15px] min-w-[15px] items-center justify-center rounded-[var(--radius-pill)] bg-[var(--color-accent-strong)] px-[3px] text-[0.6rem] font-semibold tabular-nums text-[var(--color-on-accent-strong)]"
                          >
                            {activePatrolCount}
                          </span>
                        ) : null}
                      </button>
                      {patrolPopoverOpen && !shareBusy ? (
                        <PatrolPopover
                          initialPrompt={prompt}
                          onStartPatrol={async (input) => {
                            const ok = await onStartPatrol(input);
                            if (ok !== false && prompt.trim() && input.prompt === prompt.trim()) {
                              setPrompt("");
                            }
                            return ok;
                          }}
                          onClose={() => {
                            setPatrolPopoverOpen(false);
                            (activePatrolCount > 0
                              ? patrolButtonRef.current
                              : moreButtonRef.current
                            )?.focus();
                          }}
                        />
                      ) : null}
                    </div>
                  ) : null}

                  {/* Context Ring: shown once context usage exists (or while
                      compacting / when the token-limit toast fires), and also
                      reachable inside More options. */}
                  {canCompact && (contextUsage !== null || isSummarizing || compactToastVisible) ? (
                    // On a phone the ring shows only when it needs attention
                    // (filling up, compacting, or the token-limit toast);
                    // otherwise Compact conversation in More options carries
                    // the same percentage, and the bar stays calm.
                    <div
                      data-testid="composer-context-ring"
                      className={`relative ${
                        isSummarizing ||
                        compactToastVisible ||
                        contextUsage?.severity === "warn" ||
                        contextUsage?.severity === "danger"
                          ? "inline-flex"
                          : "hidden sm:inline-flex"
                      }`}
                    >
                      {compactToastVisible && !isSummarizing ? (
                        <div
                          role="status"
                          aria-live="polite"
                          className="pointer-events-none absolute bottom-full left-1/2 mb-2 -translate-x-1/2 whitespace-nowrap rounded-md border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] px-2.5 py-1 text-[0.7rem] text-[var(--color-text-primary)] shadow-[var(--shadow-md)]"
                        >
                          Token limit hit — you should compact
                        </div>
                      ) : null}
                      <ContextRing
                        usage={contextUsage}
                        isSummarizing={isSummarizing}
                        disabled={isStreaming || isSummarizing}
                        onClick={() => setConfirmSummarize(true)}
                      />
                    </div>
                  ) : null}

                  {/* Active Auto-approve indicator: shown directly on the desktop toolbar when
                      Auto-approve is ON so the user always sees it at a glance and can
                      turn it off in a single tap (on phone viewports More options carries
                      the active dot and toggle to avoid crowding Send). */}
                  {toggleAutoApprove && isAutoApprove ? (
                    <button
                      type="button"
                      disabled={shareBusy}
                      aria-label="Auto-approve tools in this chat is on (click to turn off)"
                      data-tip-top="Auto-approve on for this chat"
                      aria-pressed={true}
                      className={`hidden sm:inline-flex ${TOOL_BTN} ${TOOL_BTN_ACTIVE}`}
                      onClick={toggleAutoApprove}
                    >
                      <Icon name="zap" className="size-3.5" />
                      <span className="sr-only">Auto-approve on</span>
                    </button>
                  ) : null}

                  {/* Active Share indicator: shown directly on the desktop toolbar only when
                      the chat is actively shared or in Multiplayer mode so the user sees who
                      can read/participate at a glance (when private, Share stays inside More
                      options so the primary bar isn't cluttered). */}
                  {isSharedAny ? (
                    <button
                      type="button"
                      data-testid={
                        moreMenuOpen
                          ? "composer-share-indicator"
                          : "composer-share-button"
                      }
                      disabled={shareDisabled}
                      aria-label={shareAriaLabel}
                      data-tip-top={shareTipText}
                      onClick={triggerShare}
                      className={`hidden sm:inline-flex ${TOOL_BTN} ${TOOL_BTN_ACTIVE}`}
                    >
                      {isMultiplayer || isTeamShared ? (
                        <TeamGlyph className="size-4 shrink-0" />
                      ) : (
                        <ShareGlyph className="size-4 shrink-0" />
                      )}
                      {alsoLink ? (
                        <span
                          aria-hidden="true"
                          data-testid="composer-share-link-badge"
                          className="absolute bottom-[0.15rem] right-[0.1rem] inline-flex size-[0.7rem] items-center justify-center rounded-full bg-[var(--color-surface-1)] text-[var(--color-accent)]"
                        >
                          <ShareGlyph className="size-[0.55rem]" />
                        </span>
                      ) : null}
                      {isMultiplayer ? (
                        <span
                          aria-hidden="true"
                          data-testid="composer-share-multiplayer-dot"
                          className="absolute right-[0.2rem] top-[0.2rem] size-1.5 rounded-full bg-[var(--color-accent)] shadow-[0_0_0_2px_var(--color-surface-1)]"
                        />
                      ) : null}
                    </button>
                  ) : null}
                </>
              );
            })()}
          </div>

          <div className="flex shrink-0 items-center gap-[0.35rem]">
            {isStreaming ? (
              <button
                aria-label="Stop generating"
                data-tip-top="Stop generating"
                className="inline-flex size-[2.1rem] shrink-0 items-center justify-center rounded-[var(--radius-pill)] border border-[var(--color-border)] text-[var(--color-text-muted)] transition hover:border-[var(--color-border-strong)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                type="button"
                onClick={() => {
                  const convKey =
                    activeConversationIdRef.current ?? PENDING_CONV_KEY;
                  abortControllersRef.current.get(convKey)?.abort();
                  if (isPendingKey(convKey)) return;
                  const cancelUrl = conversationApiUrl(convKey, "/cancel");
                  if (!cancelUrl) return;
                  void (async () => {
                    let cancelled = false;
                    try {
                      const res = await fetch(cancelUrl, { method: "POST" });
                      cancelled = res.ok;
                    } catch {
                      cancelled = false;
                    }
                    if (cancelled) cancelRecovery(convKey);
                  })();
                }}
              >
                <Icon name="stop" className="size-[1.125rem]" />
                <span className="sr-only">Stop</span>
              </button>
            ) : null}
            <button
              aria-label="Send message"
              className={`inline-flex size-[2.1rem] shrink-0 items-center justify-center rounded-[var(--radius-pill)] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed ${
                prompt.trim() &&
                !isStreaming &&
                !isUploadingAttachments &&
                !shareBusy &&
                !modelError
                  ? "bg-[image:var(--gradient-action-fill)] text-[var(--color-on-primary-fill)] hover:-translate-y-px enabled:hover:bg-[image:var(--gradient-action-fill-hover)]"
                  : "bg-[var(--color-surface-2)] text-[var(--color-text-disabled)]"
              }`}
              type="submit"
              disabled={
                !prompt.trim() ||
                isUploadingAttachments ||
                shareBusy ||
                modelError !== null
              }
              title={
                modelError
                  ? modelError.message
                  : isUploadingAttachments
                    ? "Uploading attachments…"
                    : !prompt.trim() && pendingAttachments.length > 0
                      ? "Type a message to send your attachments"
                      : isStreaming
                        ? "Queue message (runs after the current turn)"
                        : "Send message"
              }
            >
              {isUploadingAttachments ? (
                <span aria-hidden="true">…</span>
              ) : (
                <Icon name="arrow-up" className="size-4" />
              )}
            </button>
          </div>
        </div>
      </form>
      {/* Keyboard hint (the design's .composer-hint): fades in while
                the textarea is focused and out on blur, riding the fast
                motion token. Wording adapts to the send-key preference so
                Ctrl+Enter mode is self-documenting. aria-hidden — it
                duplicates what the keys already do. */}
      <p
        aria-hidden="true"
        className={`mx-auto mt-2 h-4 select-none text-center text-[0.7rem] text-[var(--color-text-muted)] transition-opacity duration-fast ${
          isComposerFocused ? "opacity-100" : "opacity-0"
        }`}
      >
        {sendOnEnter ? (
          <>
            <b className="font-semibold text-[var(--color-text-secondary)]">
              Enter
            </b>{" "}
            to send ·{" "}
            <b className="font-semibold text-[var(--color-text-secondary)]">
              Shift+Enter
            </b>{" "}
            for a new line
          </>
        ) : (
          <>
            <b className="font-semibold text-[var(--color-text-secondary)]">
              Ctrl+Enter
            </b>{" "}
            to send ·{" "}
            <b className="font-semibold text-[var(--color-text-secondary)]">
              Enter
            </b>{" "}
            for a new line
          </>
        )}
      </p>
    </>
  );
}
