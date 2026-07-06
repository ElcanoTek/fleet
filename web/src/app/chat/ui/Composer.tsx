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
// "Optional tools", "Send message"), textarea-first, send flow,
// Enter-vs-Shift+Enter, attachments, model/persona/MCP pickers, Stop — are
// unchanged.
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import { useEffect, useRef, useState } from "react";
import { PENDING_CONV_KEY } from "./workspaceHref";
import { Icon } from "./Icon";
import {
  ContextRing,
  ModelValidationBadge,
  NewModelBadge,
  PendingAttachmentChip,
  type PendingAttachment,
} from "./ChatChips";
import {
  ADVANCED_MODEL,
  ADVANCED_MODEL_LABEL,
  DEFAULT_MODEL,
  DEFAULT_MODEL_LABEL,
  labelForModel,
  tierForModel,
} from "@/app/lib/modelAliases";
import type { ContextUsage } from "@/app/lib/contextUsage";
import type { NudgeDecision } from "@/app/lib/spreadsheetNudge";
import type { Message } from "./history";
import type { MCPServerInfo, RankedModel } from "./chat-experience";
import { completeSkill, filterSkills, skillSlashQuery, type SkillInfo } from "./skillSlash";

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
// accent on hover; TOOL_BTN_ACTIVE marks an engaged control (tools with
// servers enabled, send-on-Enter on).
const TOOL_BTN =
  "relative inline-flex h-[1.95rem] w-[1.95rem] shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition hover:bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40";
const TOOL_BTN_ACTIVE =
  "bg-[color-mix(in_srgb,var(--color-accent)_14%,transparent)] text-[var(--color-accent)] hover:bg-[color-mix(in_srgb,var(--color-accent)_22%,transparent)]";

// The design's .composer-pop: the anchored popover surface every composer
// menu shares. Mobile pins it to the viewport (the anchored layout could
// push it off-screen behind the keyboard); ≥sm anchors above the trigger.
const COMPOSER_POP =
  "fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 grid gap-[0.1rem] rounded-[0.75rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-[0.3rem] shadow-[var(--shadow-md)] motion-safe:animate-pop-up sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.55rem)] sm:left-0 sm:w-[15.5rem]";

// The design's .pop-row / .pop-title / .pop-desc / .pop-sec type ramp.
const POP_ROW =
  "flex w-full items-center justify-between gap-2 rounded-[0.5rem] px-[0.6rem] py-[0.45rem] text-left text-[0.82rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";
const POP_ROW_SELECTED = "bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)]";
const POP_TITLE = "text-[0.82rem] font-medium text-[var(--color-text-primary)]";
const POP_DESC = "text-[0.7rem] text-[var(--color-text-muted)]";
const POP_SEC =
  "px-[0.6rem] pb-[0.15rem] pt-[0.35rem] text-[0.62rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]";

// The design's .mini-switch: the visual toggle knob inside a tools pop-row.
// Purely decorative — the row <button> carries the aria-pressed state.
function MiniSwitch({ on }: { on: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={`relative mt-[0.2rem] h-[0.875rem] w-6 shrink-0 rounded-full transition ${
        on
          ? "bg-[var(--color-primary)]"
          : "bg-[color-mix(in_srgb,var(--color-primary)_32%,transparent)]"
      }`}
    >
      <span
        className={`absolute left-[2px] top-[2px] size-[0.625rem] rounded-full transition ${
          on ? "translate-x-[0.625rem] bg-white" : "bg-[var(--color-text-muted)]"
        }`}
      />
    </span>
  );
}

export type ComposerProps = {
  // Textarea value + draft handling
  prompt: string;
  setPrompt: Dispatch<SetStateAction<string>>;
  promptPlaceholder: string;
  promptRef: RefObject<HTMLTextAreaElement | null>;
  submitPrompt: (submittedPrompt: string) => void | Promise<void>;

  // Sealed (lockdown) view: accent-mixed border + the explainer strip along
  // the composer's top edge. True for an active lockdown conversation, a
  // pending "new sealed chat", and lockdown-only servers.
  sealed: boolean;

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

  // Spreadsheet nudge
  spreadsheetNudge: NudgeDecision;
  setSpreadsheetNudgeDismissed: Dispatch<SetStateAction<boolean>>;

  // Persona picker
  personas: string[];
  selectedPersona: string;
  setSelectedPersona: Dispatch<SetStateAction<string>>;
  personaPickerOpen: boolean;
  setPersonaPickerOpen: Dispatch<SetStateAction<boolean>>;
  personaPickerRef: RefObject<HTMLDivElement | null>;

  // Model picker
  selectedModel: string;
  setSelectedModel: Dispatch<SetStateAction<string>>;
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
  toggleMcpServer: (conversationId: string | null, name: string) => void | Promise<void>;

  // Context ring / compaction
  activeConversationId: string | null;
  messages: Message[];
  contextUsage: ContextUsage | null;
  isSummarizing: boolean;
  compactToastVisible: boolean;
  setConfirmSummarize: Dispatch<SetStateAction<boolean>>;

  // Stop control
  activeConversationIdRef: RefObject<string | null>;
  abortControllersRef: RefObject<Record<string, AbortController>>;
  isPendingKey: (key: string | null) => boolean;
};

export function Composer({
  prompt,
  setPrompt,
  promptPlaceholder,
  promptRef,
  submitPrompt,
  sealed,
  isStreaming,
  isUploadingAttachments,
  isDraggingOver,
  setIsDraggingOver,
  dragCounterRef,
  fileInputRef,
  addAttachmentFiles,
  pendingAttachments,
  attachmentError,
  removePendingAttachment,
  spreadsheetNudge,
  setSpreadsheetNudgeDismissed,
  personas,
  selectedPersona,
  setSelectedPersona,
  personaPickerOpen,
  setPersonaPickerOpen,
  personaPickerRef,
  selectedModel,
  setSelectedModel,
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
  activeConversationId,
  messages,
  contextUsage,
  isSummarizing,
  compactToastVisible,
  setConfirmSummarize,
  activeConversationIdRef,
  abortControllersRef,
  isPendingKey,
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
  const skillMatches = skillQuery === null ? [] : filterSkills(skills, skillQuery);
  const skillHighlight = Math.min(skillIndex, Math.max(skillMatches.length - 1, 0));
  const skillPopoverOpen =
    skillMatches.length > 0 && !skillPopoverDismissed && !isStreaming;
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
      localStorage.setItem(SEND_KEY_STORAGE, sendOnEnter ? "enter" : "ctrl+enter");
    } catch {
      /* private mode / quota — preference stays session-only, which is fine */
    }
  }, [sendOnEnter]);

  // Auto-dismiss the code-paste nudge after CODE_NUDGE_TIMEOUT_MS. Each
  // appearance re-arms the timer; dismissal (click or ✕) clears state
  // directly and the cleanup here is a no-op once it's already false.
  useEffect(() => {
    if (!showCodeNudge) return;
    const timer = window.setTimeout(() => setShowCodeNudge(false), CODE_NUDGE_TIMEOUT_MS);
    return () => window.clearTimeout(timer);
  }, [showCodeNudge]);

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
              onDragOver={(event) => { event.preventDefault(); }}
              onDragLeave={() => {
                dragCounterRef.current -= 1;
                if (dragCounterRef.current === 0) setIsDraggingOver(false);
              }}
              onDrop={(event) => {
                event.preventDefault();
                dragCounterRef.current = 0;
                setIsDraggingOver(false);
                addAttachmentFiles(event.dataTransfer.files);
              }}
            >
              {isDraggingOver && (
                <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-[var(--radius-xl)] bg-[var(--color-accent)]/10">
                  <span className="text-[0.8rem] font-medium text-[var(--color-accent)]">Drop to attach</span>
                </div>
              )}
              {/* Sealed strip along the composer's top edge (the design's
                  .composer-sealed-strip). First child of the p-0 shell, so it
                  runs edge-to-edge with its top corners following the
                  container radius. */}
              {sealed ? (
                <div className="flex items-center gap-[0.45rem] rounded-t-[calc(var(--radius-xl)-1px)] border-b border-[color-mix(in_srgb,var(--color-accent)_30%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] px-[1.05rem] py-[0.45rem] text-[0.75rem] text-[var(--color-text-secondary)]">
                  <Icon name="lock" className="size-[0.8rem] shrink-0 text-[var(--color-accent)]" />
                  <span>Sealed — your data and an approved model stay in this sandbox. Nothing leaves.</span>
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
                            highlighted
                              ? "bg-[var(--color-overlay-soft)]"
                              : ""
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
                placeholder={promptPlaceholder}
                rows={1}
                suppressHydrationWarning
                value={prompt}
                onFocus={() => setIsComposerFocused(true)}
                onBlur={() => setIsComposerFocused(false)}
                onChange={(event) => {
                  const value = event.target.value;
                  setPrompt(value);
                  // Every edit resets the skill-popover highlight to the top
                  // row; an edit that leaves the slash context re-arms the Esc
                  // latch so the next "/" reopens the popover.
                  setSkillIndex(0);
                  if (skillSlashQuery(value) === null) setSkillPopoverDismissed(false);
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
                      setSkillIndex((skillHighlight - 1 + skillMatches.length) % skillMatches.length);
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
                  const shouldSend = sendOnEnter
                    ? !event.shiftKey
                    : modifierSend;
                  if (shouldSend) {
                    event.preventDefault();
                    void submitPrompt(prompt);
                  }
                }}
                onPaste={(event) => {
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
                  <span>Pasted code? Wrap in triple backticks for better formatting.</span>
                  <button
                    type="button"
                    className="font-medium text-[var(--color-accent)] hover:underline"
                    onClick={() => {
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
                      removalDisabled={isStreaming || isUploadingAttachments}
                    />
                  ))}
                  {attachmentError ? (
                    <span className="text-[0.7rem] text-[var(--color-danger)]">
                      {attachmentError}
                    </span>
                  ) : null}
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
                      {ADVANCED_MODEL_LABEL}
                    </span>
                    .
                  </span>
                  <span className="flex items-center gap-2">
                    <button
                      type="button"
                      className="rounded-full border border-[var(--color-accent)] px-2.5 py-0.5 text-[0.7rem] text-[var(--color-text-primary)] transition hover:bg-[var(--color-accent)] hover:text-[var(--color-surface-1)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                      disabled={isStreaming}
                      onClick={() => {
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
                className="hidden"
                onChange={(event) => {
                  addAttachmentFiles(event.target.files);
                  // Reset so picking the same file twice in a row still fires onChange.
                  event.target.value = "";
                }}
              />

              <div className="flex items-center justify-between gap-2 px-[0.7rem] pt-[0.45rem] pb-[0.6rem]">
                <div className="flex min-w-0 flex-wrap items-center gap-[0.35rem] overflow-visible">
                  {/* Model chip (the design's .model-chip): icon + current
                      model + caret opening the .composer-pop listbox. The
                      search field at the top of the popover preserves the
                      old inline input's type-to-search-the-catalog and free
                      slug entry; Esc closes and returns focus to the chip. */}
                  <div
                    ref={modelPickerRef}
                    className="relative inline-flex"
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
                      disabled={isStreaming}
                      title={
                        modelError
                          ? modelError.message
                          : `OpenRouter model slug — aliases: ${DEFAULT_MODEL_LABEL} → ${DEFAULT_MODEL}, ${ADVANCED_MODEL_LABEL} → ${ADVANCED_MODEL}`
                      }
                      className={`inline-flex h-[1.95rem] shrink-0 items-center gap-[0.4rem] rounded-[var(--radius-md)] py-[0.3rem] pl-[0.6rem] pr-[0.5rem] text-[0.78rem] font-medium transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40 ${
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
                        className={`size-[0.85rem] ${modelError ? "" : "text-[var(--color-accent)]"}`}
                      />
                      <span className="max-w-[9rem] truncate">{labelForModel(selectedModel)}</span>
                      <Icon name="selector" className="size-[0.8rem] text-[var(--color-text-muted)]" />
                    </button>
                    {modelPickerOpen && !isStreaming ? (
                      <div className={COMPOSER_POP}>
                        <input
                          ref={modelInputRef}
                          type="text"
                          autoFocus
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
                            // Typing both filters the catalog and — as with
                            // the old inline input — keeps free slug entry
                            // working: the raw text is the selected model
                            // until a row is picked.
                            setSelectedModel(event.target.value);
                            setModelSearchQuery(event.target.value);
                            setModelHighlight(0);
                          }}
                          onKeyDown={(event) => {
                            const count = filteredRankedModels.length;
                            if (event.key === "ArrowDown" && count > 0) {
                              event.preventDefault();
                              setModelHighlight((h) => (Math.min(h, count - 1) + 1) % count);
                            } else if (event.key === "ArrowUp" && count > 0) {
                              event.preventDefault();
                              setModelHighlight((h) => (Math.min(h, count - 1) - 1 + count) % count);
                            } else if (event.key === "Enter") {
                              event.preventDefault();
                              const pick = filteredRankedModels[Math.min(modelHighlight, count - 1)];
                              if (pick) setSelectedModel(pick.slug);
                              closeModelPicker(true);
                            }
                          }}
                        />
                        <div
                          id="composer-model-listbox"
                          role="listbox"
                          aria-label="Model options"
                          className="grid max-h-72 gap-[0.1rem] overflow-y-auto"
                        >
                          {isLoadingRankedModels || (isLoadingCatalog && modelSearchQuery.trim() !== "") ? (
                            <div className="px-[0.6rem] py-[0.45rem] text-[0.74rem] text-[var(--color-text-muted)]">Loading...</div>
                          ) : filteredRankedModels.length === 0 ? (
                            <div className="px-[0.6rem] py-[0.45rem] text-[0.74rem] text-[var(--color-text-muted)]">No matches</div>
                          ) : (
                            filteredRankedModels.map((model, i) => {
                              // One pill per row, picked from a strict
                              // hierarchy so the listbox stays uncluttered:
                              //   recommended > tested > ✨ new > experimental
                              // recommended = tier slug (default/advanced),
                              // styled as the design's .rec-tag.
                              const tier = model.slug ? tierForModel(model.slug) : null;
                              const isTier = tier === "default" || tier === "advanced";
                              const isFresh = isNewlyReleased(model.created);
                              let pill: ReactNode = null;
                              if (isTier) {
                                pill = (
                                  <span className="shrink-0 rounded-[var(--radius-pill)] border border-[color-mix(in_srgb,var(--color-success)_40%,transparent)] px-[0.45rem] py-[0.14rem] text-[0.58rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-success)]">
                                    Recommended
                                  </span>
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
                                i === Math.min(modelHighlight, filteredRankedModels.length - 1);
                              return (
                                <button
                                  key={model.slug || "__default__"}
                                  id={`composer-model-opt-${i}`}
                                  type="button"
                                  role="option"
                                  aria-selected={isSelected}
                                  title={model.slug ? `${model.name} (${model.slug})` : "Use the server-configured default model"}
                                  className={`${POP_ROW} ${isSelected ? POP_ROW_SELECTED : ""} ${
                                    isHighlighted && !isSelected ? "bg-[var(--rail-hover)]" : ""
                                  }`}
                                  onClick={() => {
                                    setSelectedModel(model.slug);
                                    closeModelPicker(true);
                                  }}
                                >
                                  <span className="grid min-w-0 gap-[0.1rem]">
                                    <span className={`${POP_TITLE} truncate`}>{model.name}</span>
                                    {model.slug ? (
                                      <span className={`${POP_DESC} truncate`}>{model.slug}</span>
                                    ) : null}
                                  </span>
                                  {pill}
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
                    className="mx-[0.15rem] h-[1.1rem] w-px shrink-0 bg-[var(--color-border-strong)]"
                  />
                  {(() => {
                    // Persona is locked server-side once a conversation has any
                    // turns, so once the chat is underway the picker is read-only
                    // noise. Hide it entirely after the first turn (and during
                    // the very first stream) to keep the composer toolbar tidy.
                    const personaLocked =
                      isStreaming || (activeConversationId !== null && messages.length > 0);
                    if (personaLocked) return null;
                    const personaOptions = personas.length > 0 ? personas : [selectedPersona];
                    const formatPersona = (p: string) =>
                      p.charAt(0).toUpperCase() + p.slice(1);
                    return (
                      <div
                        ref={personaPickerRef}
                        className="relative inline-flex"
                        onKeyDown={(event) => {
                          if (event.key === "Escape" && personaPickerOpen) {
                            event.stopPropagation();
                            setPersonaPickerOpen(false);
                            personaButtonRef.current?.focus();
                          }
                        }}
                      >
                        {/* Icon-only .tool-btn (the design has no persona
                            control; it joins the icon cluster per review).
                            The active persona is announced via the
                            aria-label and shown in the hover tip; the
                            popover marks it with the selected state. */}
                        <button
                          ref={personaButtonRef}
                          type="button"
                          aria-haspopup="listbox"
                          aria-expanded={personaPickerOpen}
                          aria-label={`Persona — ${formatPersona(selectedPersona)}`}
                          data-tip-top={`Persona — ${formatPersona(selectedPersona)}`}
                          className={`${TOOL_BTN} ${personaPickerOpen ? TOOL_BTN_ACTIVE : ""}`}
                          onClick={() => setPersonaPickerOpen((open) => !open)}
                        >
                          <Icon name="persona" className="size-3.5" />
                        </button>
                        {personaPickerOpen ? (
                          <div role="listbox" aria-label="Persona" className={COMPOSER_POP}>
                            {personaOptions.map((p) => {
                              const selected = p === selectedPersona;
                              return (
                                <button
                                  key={p}
                                  type="button"
                                  role="option"
                                  aria-selected={selected}
                                  className={`${POP_ROW} ${selected ? POP_ROW_SELECTED : ""}`}
                                  onClick={() => {
                                    setSelectedPersona(p);
                                    setPersonaPickerOpen(false);
                                    personaButtonRef.current?.focus();
                                  }}
                                >
                                  <span className={`${POP_TITLE} truncate`}>{formatPersona(p)}</span>
                                  {selected ? (
                                    <span
                                      aria-hidden="true"
                                      className="size-1.5 shrink-0 rounded-full bg-[var(--color-accent)]"
                                    />
                                  ) : null}
                                </button>
                              );
                            })}
                          </div>
                        ) : null}
                      </div>
                    );
                  })()}
                  <button
                    type="button"
                    aria-label="Attach files"
                    data-tip-top="Attach files"
                    className={TOOL_BTN}
                    disabled={isStreaming || isUploadingAttachments}
                    onClick={() => fileInputRef.current?.click()}
                  >
                    <Icon name="paperclip" className="size-3.5" />
                  </button>
                  {mcpServers.length > 0 ? (
                    <div
                      ref={mcpPickerRef}
                      className="relative inline-flex"
                      onKeyDown={(event) => {
                        if (event.key === "Escape" && mcpPickerOpen) {
                          event.stopPropagation();
                          setMcpPickerOpen(false);
                          mcpButtonRef.current?.focus();
                        }
                      }}
                    >
                      {(() => {
                        const enabledCount = mcpServers.filter((s) => s.enabled).length;
                        return (
                          <button
                            ref={mcpButtonRef}
                            type="button"
                            aria-label="Optional tools"
                            aria-haspopup="true"
                            aria-expanded={mcpPickerOpen}
                            disabled={isStreaming}
                            data-tip-top="Tools & MCP servers"
                            className={`${TOOL_BTN} ${enabledCount > 0 ? TOOL_BTN_ACTIVE : ""}`}
                            onClick={() => {
                              const next = !mcpPickerOpen;
                              setMcpPickerOpen(next);
                              // Pre-chat: the preview catalog loaded at
                              // startup is already in state — no per-conv
                              // row to fetch yet.
                              if (next && activeConversationId) {
                                void loadMcpServerCatalog(activeConversationId);
                              }
                            }}
                          >
                            <Icon name="wrench" className="size-3.5" />
                            {/* The design's .tool-badge: enabled-count bubble
                                pinned to the button's top-right corner. */}
                            {enabledCount > 0 ? (
                              <span className="pointer-events-none absolute right-0 top-0 inline-flex h-[0.875rem] min-w-[0.875rem] -translate-y-1/2 translate-x-1/2 items-center justify-center rounded-[var(--radius-pill)] bg-[var(--color-accent)] px-[2px] font-[family-name:var(--font-code)] text-[0.56rem] font-medium tabular-nums text-[var(--color-bg)]">
                                {enabledCount}
                              </span>
                            ) : null}
                          </button>
                        );
                      })()}
                      {mcpPickerOpen && !isStreaming ? (
                        <div className={COMPOSER_POP}>
                          {/* One .pop-sec section: fleet's composer catalog
                              only carries optional MCP servers (the design's
                              "Built-in" section is mock content — always-on
                              tools are not per-conversation toggles here). */}
                          <div className={POP_SEC}>MCP servers</div>
                          <div className="grid max-h-80 gap-[0.1rem] overflow-y-auto">
                            {isLoadingMcpServers ? (
                              <div className="px-[0.6rem] py-[0.45rem] text-[0.74rem] text-[var(--color-text-muted)]">Loading...</div>
                            ) : (
                              mcpServers.map((server) => (
                                <button
                                  key={server.name}
                                  type="button"
                                  aria-pressed={server.enabled}
                                  title={(server.tools ?? []).join(", ")}
                                  className={`${POP_ROW} items-start`}
                                  onClick={() => {
                                    void toggleMcpServer(activeConversationId, server.name);
                                  }}
                                >
                                  <span className="grid min-w-0 gap-[0.1rem]">
                                    <span className="flex items-center gap-1.5">
                                      <span className={POP_TITLE}>{server.display_name || server.name}</span>
                                      {server.beta ? (
                                        <span
                                          className="rounded-sm border border-[var(--color-border-strong)] px-1 py-px text-[0.55rem] font-semibold uppercase tracking-wider text-[var(--color-text-muted)]"
                                          title="This connector is in beta — it works but still has rough edges."
                                        >
                                          beta
                                        </span>
                                      ) : null}
                                    </span>
                                    {server.description ? (
                                      <span className={`${POP_DESC} leading-snug`}>{server.description}</span>
                                    ) : null}
                                    <span className="text-[0.65rem] text-[var(--color-text-muted)]">
                                      {server.tool_count} tool{server.tool_count === 1 ? "" : "s"}
                                    </span>
                                  </span>
                                  <MiniSwitch on={server.enabled} />
                                </button>
                              ))
                            )}
                          </div>
                        </div>
                      ) : null}
                    </div>
                  ) : null}
                  {activeConversationId && messages.length >= 2 ? (
                    <div className="relative inline-flex">
                      {/* One-shot toast above the ring. Absolute so the */}
                      {/* toolbar layout doesn't reflow as it appears /  */}
                      {/* disappears. pointer-events-none so it can      */}
                      {/* never steal a click meant for the ring below. */}
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
                </div>

                <div className="flex items-center gap-[0.35rem]">
                  {isStreaming ? (
                    <button
                      className="text-[0.6875rem] font-medium text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)]"
                      type="button"
                      onClick={() => {
                        // Tell the server to actually stop the turn.
                        // The server now keeps generating after the SSE
                        // drops (so phone-lock + long turns don't lose
                        // work), so an explicit cancel signal is the
                        // only thing that brings the work to a halt.
                        // Per-conv: only the chat the user is currently
                        // looking at — other in-flight chats keep going.
                        const convKey =
                          activeConversationIdRef.current ?? PENDING_CONV_KEY;
                        if (!isPendingKey(convKey)) {
                          void fetch(`/api/conversations/${convKey}/cancel`, {
                            method: "POST",
                          }).catch(() => {
                            /* non-fatal — server will time out the turn anyway */
                          });
                        }
                        abortControllersRef.current[convKey]?.abort();
                      }}
                    >
                      Stop
                    </button>
                  ) : null}
                  {/* Send-key preference toggle (issue #315), restyled as the
                      design's .tool-btn with the return-key glyph; the active
                      state marks "Send on Enter" on. The mode is announced
                      via aria-label and the hover tip; clicking flips +
                      persists it. Sits right next to Send so the toggle is
                      in the same glance as the key it configures. */}
                  <button
                    type="button"
                    aria-label={
                      sendOnEnter
                        ? "Send on Enter (click to switch to Ctrl+Enter)"
                        : "Send on Ctrl+Enter (click to switch to Enter)"
                    }
                    data-tip-top={sendOnEnter ? "Send on Enter" : "Send on Ctrl+Enter"}
                    aria-pressed={sendOnEnter}
                    className={`${TOOL_BTN} ${sendOnEnter ? TOOL_BTN_ACTIVE : ""}`}
                    onClick={() => setSendOnEnter((v) => !v)}
                  >
                    <Icon name="return-key" className="size-3.5" />
                    <span className="sr-only">{sendOnEnter ? "Enter" : "Ctrl+Enter"}</span>
                  </button>
                  {/* The design's .send-btn: a 2.1rem circle that lights up
                      with the action gradient once there is content to send
                      (the image: hint keeps the gradient token in
                      background-image). */}
                  <button
                    aria-label="Send message"
                    className={`inline-flex size-[2.1rem] shrink-0 items-center justify-center rounded-[var(--radius-pill)] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed ${
                      prompt.trim() && !isStreaming && !isUploadingAttachments
                        ? "bg-[image:var(--gradient-action-primary)] text-white hover:-translate-y-px"
                        : "bg-[var(--color-surface-2)] text-[var(--color-text-disabled)]"
                    }`}
                    type="submit"
                    disabled={!prompt.trim() || isStreaming || isUploadingAttachments}
                    title={isUploadingAttachments ? "Uploading attachments…" : "Send message"}
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
                  <b className="font-semibold text-[var(--color-text-secondary)]">Enter</b> to send ·{" "}
                  <b className="font-semibold text-[var(--color-text-secondary)]">Shift+Enter</b> for a new line
                </>
              ) : (
                <>
                  <b className="font-semibold text-[var(--color-text-secondary)]">Ctrl+Enter</b> to send ·{" "}
                  <b className="font-semibold text-[var(--color-text-secondary)]">Enter</b> for a new line
                </>
              )}
            </p>
    </>
  );
}
