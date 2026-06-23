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

import Image from "next/image";
import type { ReactElement, ReactNode } from "react";
import { Children, isValidElement, memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
// Syntax highlighter: Prism light build so we only ship the languages
// we actually render. Language grammars are registered eagerly below.
import { PrismLight as SyntaxHighlighter } from "react-syntax-highlighter";
import pythonGrammar from "react-syntax-highlighter/dist/esm/languages/prism/python";
import bashGrammar from "react-syntax-highlighter/dist/esm/languages/prism/bash";
import jsonGrammar from "react-syntax-highlighter/dist/esm/languages/prism/json";
import yamlGrammar from "react-syntax-highlighter/dist/esm/languages/prism/yaml";
SyntaxHighlighter.registerLanguage("python", pythonGrammar);
SyntaxHighlighter.registerLanguage("bash", bashGrammar);
SyntaxHighlighter.registerLanguage("shell", bashGrammar);
SyntaxHighlighter.registerLanguage("json", jsonGrammar);
SyntaxHighlighter.registerLanguage("yaml", yamlGrammar);
import { deriveConversationTitle } from "@/app/lib/title";
import {
  ADVANCED_MODEL,
  ADVANCED_MODEL_LABEL,
  DEFAULT_MODEL,
  DEFAULT_MODEL_LABEL,
  labelForModel,
  tierForModel,
  type ModelTier,
} from "@/app/lib/modelAliases";
import {
  computeContextUsage,
  formatContextUsage,
  shouldShowDegradationCaption,
  type ContextUsage,
} from "@/app/lib/contextUsage";
import { parseSseChunk, type ServerEvent } from "@/app/lib/sse";
import { decideSpreadsheetNudge } from "@/app/lib/spreadsheetNudge";
import { LoadingLogo } from "./LoadingLogo";
import { EmptyStatePrompts, ProtocolPillForm } from "./EmptyStatePrompts";
import { RuntimePicker } from "./RuntimePicker";
import { PermissionCard } from "./PermissionCard";
import { getPill } from "./protocolPills";
import { useClientConfig } from "@/app/lib/useClientConfig";
import {
  applyModelRequired,
  applyRetryNotice,
  cachedPercent,
  clearRetryNotice,
  conversationTotals,
  historyToMessages,
  humanToolLabel,
  parsePythonStream,
  prettyToolName,
  safePretty,
  shortModelName,
  toolIcon,
  type Approval,
  type ApprovalStatus,
  type HistoryEntry,
  type MemoryProposal,
  type Message,
  type ModelRequiredEventPayload,
  type PermissionOption,
  type PermissionStatus,
  type PythonStream,
  type RetryEventPayload,
  type ToolCall,
  type ToolCallState,
  type TurnSummary,
} from "./history";
import { PENDING_CONV_KEY, resolveWorkspaceHref } from "./workspaceHref";

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

type ConversationSummary = {
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
  // runtime is the per-conversation execution flavor (fleet's ACP runtime
  // selection). Empty = the bundle default. Drives the flavor picker.
  runtime?: string;
};

type ServerConfig = {
  // lockdownAvailable: lockdown UI should be exposed (sandbox image
  // configured on the server). When false, no lockdown button at all.
  lockdownAvailable: boolean;
  // lockdownOnly: every chat is forcibly lockdown — hide the regular
  // "+" button, only show the lockdown one. For sensitive deploys.
  lockdownOnly: boolean;
  lockdownAllowedModels: string[];
};

type PendingDeleteConversation = {
  id: string;
  title: string;
};

type PersonasResponse = {
  personas: string[];
  default: string;
};

// PendingAttachment describes a file the user has picked to send with the
// next turn. We carry the live File object for display (chip name/size
// before upload) and, once the upload finishes, the server-returned path
// the /api/chat body needs to include so chat-server can hand it to the
// agent. "uploading" / "done" / "error" reflects the per-file state when
// multiple files are in flight.
type PendingAttachment = {
  clientId: string;
  file: File;
  status: "pending" | "uploading" | "done" | "error";
  name: string;
  size: number;
  mime: string;
  // Populated once the server accepts the upload.
  serverPath?: string;
  errorMessage?: string;
};

type UploadedAttachmentMeta = {
  name: string;
  path: string;
  size: number;
  mime?: string;
};

type RankedModel = {
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

type UserMemory = {
  id: string;
  content: string;
  source: "manual" | "chat" | string;
  created_at: number;
  updated_at: number;
};

// ── streaming helpers ────────────────────────────────────────────────────

const minimumThinkingMs = 250;
const streamIdleTimeoutMs = 300000;

function Icon({ name, className }: { name: string; className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
      <use href={`/icons/core-icons.svg#${name}`} />
    </svg>
  );
}

// ModelValidationBadge renders a small pill next to non-tier rows in the
// model picker. "tested" means we've validated end-to-end with our tools
// + system prompt; "experimental" means it should still work but no
// guarantees. Three-tier visual hierarchy with the recommended pill on
// tier rows: solid accent fill > translucent accent (tested) > muted
// outline (experimental).
function ModelValidationBadge({ tier }: { tier: ModelTier }) {
  const tone =
    tier === "tested"
      ? "border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 text-[var(--color-accent)]"
      : "border-[var(--color-border-strong)] text-[var(--color-text-muted)]";
  return (
    <span
      className={`shrink-0 rounded-full border px-1.5 py-0 text-[0.6rem] font-medium leading-4 tabular-nums ${tone}`}
    >
      {tier}
    </span>
  );
}

// "✨ new" pill for entries listed on OpenRouter within the last
// NEW_MODEL_WINDOW_DAYS. Uses --color-success (green) so it sits
// alongside the accent-fill `recommended` pill without competing for
// the same visual slot.
function NewModelBadge() {
  return (
    <span
      className="shrink-0 rounded-full border border-[var(--color-success-border)] bg-[var(--color-success)]/15 px-1.5 py-0 text-[0.6rem] font-semibold leading-4 tabular-nums text-[var(--color-success)]"
      title="Listed on OpenRouter within the last two weeks"
    >
      ✨ new
    </span>
  );
}

// ── markdown renderer ────────────────────────────────────────────────────

// Models sometimes paste a full <!DOCTYPE html>…</html> document directly
// into prose instead of wrapping it in a ```html fence (which the system
// prompt explicitly tells them to do). ReactMarkdown is configured with
// only remarkGfm — no rehype-raw — so block-level HTML is silently
// dropped, and the user sees a mysterious gap with no preview and no
// source to copy. Auto-wrap any unfenced HTML document in a ```html
// fence so the existing InlineHtmlPreview mounts a sandboxed iframe.
//
// Exported for the markdown unit tests.
export function autoFenceRawHtmlDocument(content: string): string {
  if (!/<!DOCTYPE\s+html|<html[\s>]/i.test(content)) return content;
  const lines = content.split("\n");
  const out: string[] = [];
  let inFence = false;
  let inHtml = false;
  for (const line of lines) {
    if (/^\s*```/.test(line)) {
      if (inHtml) {
        out.push("```");
        inHtml = false;
      }
      inFence = !inFence;
      out.push(line);
      continue;
    }
    if (inFence) {
      out.push(line);
      continue;
    }
    if (!inHtml && /^\s*(<!DOCTYPE\s+html|<html[\s>])/i.test(line)) {
      out.push("```html");
      out.push(line);
      inHtml = true;
      continue;
    }
    out.push(line);
    if (inHtml && /<\/html>\s*$/i.test(line)) {
      out.push("```");
      inHtml = false;
    }
  }
  if (inHtml) out.push("```");
  return out.join("\n");
}

// Exported for unit tests in chat-experience.markdown.test.tsx; production
// callers continue to use it through the in-module references below.
export function renderAssistantContent(
  content: string,
  isStreaming = false,
  conversationId: string | null = null,
): ReactNode {
  if (!content.trim()) {
    return null;
  }

  const normalizedContent = autoFenceRawHtmlDocument(
    content
      .replace(/(^|\n)\*\*([^*\n:]+)\*\*(?=\s*$|\n)/g, "$1**$2**")
      .replace(/(^|\n)\*\*([^*\n:]+)(?=\n|$)/g, "$1$2")
      .replace(/(^|\n)([A-Za-z][A-Za-z /]+):\s*`([^`]+)`/g, "$1**$2:** $3"),
  );

  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      // Preserve the `sandbox:` scheme so the <a>/<img> interceptors below
      // can rewrite it to the workspace API. ReactMarkdown's
      // defaultUrlTransform strips any scheme outside its safe list
      // (http/https/mailto/tel/…), which silently empties a
      // `sandbox:/opt/chat/workspace/<id>/file` href BEFORE our renderer
      // runs — so the sandbox-stripping logic in resolveWorkspaceHref never
      // got a chance to fire and the link rendered with no href. Models
      // still emit these hallucinated sandbox paths, so let them through
      // here and resolve them downstream; every other URL keeps the default
      // sanitization.
      urlTransform={(url) => (/^sandbox:/i.test(url) ? url : defaultUrlTransform(url))}
      components={{
        h1: ({ children }) => <h1 className="assistant-markdown-h1">{children}</h1>,
        h2: ({ children }) => <h2 className="assistant-markdown-h2">{children}</h2>,
        h3: ({ children }) => <h3 className="assistant-markdown-h3">{children}</h3>,
        p: ({ children }) => <p className="assistant-markdown-p">{children}</p>,
        ul: ({ children }) => <ul className="assistant-markdown-ul">{children}</ul>,
        ol: ({ children }) => <ol className="assistant-markdown-ol">{children}</ol>,
        li: ({ children }) => <li className="assistant-markdown-li">{children}</li>,
        hr: () => <hr className="assistant-markdown-hr" />,
        table: ({ children }) => (
          <div className="assistant-markdown-table-shell">
            <table className="assistant-markdown-table">{children}</table>
          </div>
        ),
        thead: ({ children }) => <thead className="assistant-markdown-thead">{children}</thead>,
        th: ({ children }) => <th className="assistant-markdown-th">{children}</th>,
        td: ({ children }) => <td className="assistant-markdown-td">{children}</td>,
        code: ({ children, className }) => {
          const isBlock = Boolean(className);
          if (isBlock) {
            return <code className="assistant-markdown-code-block">{children}</code>;
          }
          return <code className="assistant-markdown-code-inline">{children}</code>;
        },
        pre: ({ children }) => {
          // Intercept ```html fences and render them as a sandboxed
          // preview so the agent can just emit HTML in a code block and
          // have it render. Anything else falls through to the default
          // <pre> styling, wrapped in a toolbar that exposes a copy
          // button and the language tag.
          const codeChild = Children.toArray(children).find((c) =>
            isValidElement(c) && typeof (c as ReactElement<{ className?: string }>).props.className === "string",
          ) as ReactElement<{ className?: string; children?: ReactNode }> | undefined;
          let language: string | null = null;
          let rawText = "";
          if (codeChild) {
            const cls = codeChild.props.className ?? "";
            const langMatch = cls.match(/language-([^\s]+)/i);
            if (langMatch) language = langMatch[1].toLowerCase();
            rawText = typeof codeChild.props.children === "string"
              ? codeChild.props.children
              : Children.toArray(codeChild.props.children).join("");
            if (language === "html") {
              return <InlineHtmlPreview html={rawText.replace(/\n$/, "")} isStreaming={isStreaming} conversationId={conversationId} />;
            }
          }
          const copyText = rawText.replace(/\n$/, "");
          return (
            <div className="assistant-markdown-pre-wrapper">
              <div className="assistant-markdown-pre-toolbar">
                <span className="assistant-markdown-pre-lang">{language ?? ""}</span>
                <CopyButton text={copyText} title="Copy code to clipboard" variant="compact" />
              </div>
              <pre className="assistant-markdown-pre">{children}</pre>
            </div>
          );
        },
        // Rewrite relative <img> srcs to the per-conversation workspace
        // file API. The agent saves a chart with `plt.savefig('chart.png')`
        // and writes `![Chart](chart.png)` in its reply; without this
        // rewrite the browser would request `/chart.png` (404) instead of
        // the real workspace path. data: URLs and absolute http(s) URLs
        // pass through unchanged so e.g. inline base64 still works and
        // the agent can still link to public images.
        img: ({ src, alt, title }) => {
          const { href } = resolveWorkspaceHref(typeof src === "string" ? src : "", conversationId);
          return <WorkspaceImage src={href} alt={alt ?? ""} title={title ?? undefined} />;
        },
        // Same rewrite for <a href>: when the agent writes
        // `[Deck.pptx](Deck.pptx)` after producing the file via an MCP
        // tool, the browser would otherwise try to navigate to a sibling
        // path of the chat page and 404. Rewriting to the workspace API
        // makes the link actually serve the file. Workspace links also
        // get a `download` attribute so the browser saves the file
        // instead of trying to render binary content inline, and external
        // links open in a new tab so we don't lose the chat state.
        // Visible styling (color + underline via .assistant-markdown-link)
        // is what makes the link recognizable as a link at all — without
        // it, react-markdown's bare <a> inherits body color and looks
        // identical to surrounding text.
        a: ({ href, title, children }) => {
          const { href: resolved, isWorkspaceFile, downloadFilename } = resolveWorkspaceHref(
            typeof href === "string" ? href : "",
            conversationId,
          );
          const isExternal = /^https?:\/\//i.test(resolved);
          const extraProps: { target?: string; rel?: string; download?: string } = {};
          if (isWorkspaceFile) {
            // Pass the original basename so the browser saves with the
            // name the agent referenced, not a percent-encoded URL slice.
            extraProps.download = downloadFilename || "";
          } else if (isExternal) {
            extraProps.target = "_blank";
            extraProps.rel = "noopener noreferrer";
          }
          return (
            <a
              className="assistant-markdown-link"
              href={resolved || undefined}
              title={title ?? undefined}
              {...extraProps}
            >
              {children}
            </a>
          );
        },
        strong: ({ children }) => <strong className="assistant-markdown-strong">{children}</strong>,
        em: ({ children }) => <em className="assistant-markdown-em">{children}</em>,
      }}
    >
      {normalizedContent}
    </ReactMarkdown>
  );
}

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
  // Per-conv composer state — promptByConv / pendingAttachmentsByConv /
  // attachmentErrorByConv / uploadingConvs. These used to be global,
  // which meant typing in chat A then switching to chat B leaked A's
  // draft + queued files into B's composer. They're keyed by
  // currentConvKey (real conv id or the PENDING sentinel for the empty
  // new-chat view) so each chat keeps its own draft + uploads + errors.
  // Setters are derived below and use closure-captured currentConvKey
  // so an async submit in conv A clears A's slot even if the user
  // navigated to B in the meantime.
  const [promptByConv, setPromptByConv] = useState<Record<string, string>>({});
  const [pendingAttachmentsByConv, setPendingAttachmentsByConv] = useState<
    Record<string, PendingAttachment[]>
  >({});
  const [attachmentErrorByConv, setAttachmentErrorByConv] = useState<
    Record<string, string | null>
  >({});
  // uploadingConvs is the set of conv keys with an in-flight attachment
  // upload. Used so the send button + attachment-removal chips disable
  // only for the conv whose upload is running, not for an unrelated
  // chat the user navigates to.
  const [uploadingConvs, setUploadingConvs] = useState<Set<string>>(
    () => new Set<string>(),
  );
  const uploadingConvsRef = useRef<Set<string>>(new Set<string>());
  const markConvUploading = (key: string) => {
    if (uploadingConvsRef.current.has(key)) return;
    uploadingConvsRef.current.add(key);
    setUploadingConvs(new Set(uploadingConvsRef.current));
  };
  const markConvUploadDone = (key: string) => {
    if (!uploadingConvsRef.current.has(key)) return;
    uploadingConvsRef.current.delete(key);
    setUploadingConvs(new Set(uploadingConvsRef.current));
  };
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [theme, setTheme] = useState<"light" | "dark">("dark");
  // showStats gates the whole per-turn "details" block: the cost /
  // latency / tokens / model chip, the conversation-wide totals chip,
  // AND the execution trail (tool pills + python stream output). One
  // switch for every technical surface so non-developers get a clean
  // chat and power users can flip everything on at once. Persisted to
  // localStorage so the preference survives reloads.
  const [showStats, setShowStats] = useState(false);
  // streamingConvs tracks every conversation slot that currently has a
  // turn in flight — keyed by conv id (or PENDING_CONV_KEY for a
  // brand-new chat whose server id we haven't heard back yet). Multiple
  // entries = multiple chats running in parallel. The sidebar reads this
  // to paint a "working" dot next to each in-flight chat, and the active
  // conv's composer uses the derived `isStreaming` below to gate input.
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
  const [crossfadingMessageIds, setCrossfadingMessageIds] = useState<number[]>([]);
  const [userEmail, setUserEmail] = useState("");
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  const [activeConversationId, setActiveConversationId] = useState<string | null>(null);
  // The slot identifier the rest of the UI hangs off of: the active
  // conv id when one is loaded, the PENDING sentinel when the user is
  // on the empty new-chat view, or a per-submission pending key during
  // the brief window between submit and the server's "conversation"
  // SSE event. messagesByConv, promptByConv, pendingAttachmentsByConv,
  // etc. all key on this string.
  const currentConvKey = activeConversationId ?? PENDING_CONV_KEY;
  // Derived from streamingConvs: true when the conv the user is currently
  // looking at has a turn in flight. Drives the composer disabled states,
  // Stop button visibility, auto-scroll behavior, etc. Other conversations
  // may also be streaming simultaneously — see streamingConvs and the
  // sidebar dot indicator.
  const isStreaming = streamingConvs.has(currentConvKey);
  // Composer derivations. Each setter mutates the per-conv Record under
  // the currentConvKey captured at *render* time, which means an async
  // submit closure keeps writing to the slot it was launched from even
  // if the user has since navigated to another chat.
  const EMPTY_PENDING_ATTACHMENTS: readonly PendingAttachment[] = [];
  const prompt = promptByConv[currentConvKey] ?? "";
  const pendingAttachments =
    pendingAttachmentsByConv[currentConvKey] ??
    (EMPTY_PENDING_ATTACHMENTS as PendingAttachment[]);
  const attachmentError = attachmentErrorByConv[currentConvKey] ?? null;
  const isUploadingAttachments = uploadingConvs.has(currentConvKey);
  const setPrompt: React.Dispatch<React.SetStateAction<string>> = (next) => {
    setPromptByConv((prev) => {
      const old = prev[currentConvKey] ?? "";
      const value =
        typeof next === "function"
          ? (next as (s: string) => string)(old)
          : next;
      if (value === old) return prev;
      const out = { ...prev };
      if (value === "") delete out[currentConvKey];
      else out[currentConvKey] = value;
      return out;
    });
  };
  const setPromptForKey = (key: string, value: string) => {
    setPromptByConv((prev) => {
      const old = prev[key] ?? "";
      if (value === old) return prev;
      const out = { ...prev };
      if (value === "") delete out[key];
      else out[key] = value;
      return out;
    });
  };
  const setPendingAttachments: React.Dispatch<
    React.SetStateAction<PendingAttachment[]>
  > = (next) => {
    setPendingAttachmentsByConv((prev) => {
      const old = prev[currentConvKey] ?? [];
      const value =
        typeof next === "function"
          ? (next as (a: PendingAttachment[]) => PendingAttachment[])(old)
          : next;
      if (value === old) return prev;
      const out = { ...prev };
      if (value.length === 0) delete out[currentConvKey];
      else out[currentConvKey] = value;
      return out;
    });
  };
  const setPendingAttachmentsForKey = (key: string, value: PendingAttachment[]) => {
    setPendingAttachmentsByConv((prev) => {
      const out = { ...prev };
      if (value.length === 0) delete out[key];
      else out[key] = value;
      return out;
    });
  };
  const setAttachmentError: React.Dispatch<
    React.SetStateAction<string | null>
  > = (next) => {
    setAttachmentErrorByConv((prev) => {
      const old = prev[currentConvKey] ?? null;
      const value =
        typeof next === "function"
          ? (next as (s: string | null) => string | null)(old)
          : next;
      if (value === old) return prev;
      const out = { ...prev };
      if (value === null) delete out[currentConvKey];
      else out[currentConvKey] = value;
      return out;
    });
  };
  const setAttachmentErrorForKey = (key: string, value: string | null) => {
    setAttachmentErrorByConv((prev) => {
      const out = { ...prev };
      if (value === null) delete out[key];
      else out[key] = value;
      return out;
    });
  };
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
  const { branding, pills, runtimes, defaultRuntime } = useClientConfig();
  // selectedRuntime is the active conversation's runtime flavor ("" = use the
  // bundle default). Synced from the conversation on load; persisted with the
  // turn via the chat request body + the dedicated /runtime endpoint.
  const [selectedRuntime, setSelectedRuntime] = useState<string>("");
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
  // Optional MCP servers the user can toggle on per-conversation. The catalog
  // (name, description, tool count, enabled) comes from the chat-server and
  // is fetched when a conversation is loaded. Non-optional servers are not
  // represented here — they're always on.
  type MCPServerInfo = {
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
  const [mcpServers, setMcpServers] = useState<MCPServerInfo[]>([]);
  const [mcpPickerOpen, setMcpPickerOpen] = useState(false);
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
  const activeConversation = useMemo(
    () => conversations.find((c) => c.id === activeConversationId) ?? null,
    [conversations, activeConversationId],
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
  const [memoryDraft, setMemoryDraft] = useState("");
  const [editingMemoryId, setEditingMemoryId] = useState<string | null>(null);
  const [memoryError, setMemoryError] = useState<string | null>(null);
  const [isLoadingMemories, setIsLoadingMemories] = useState(false);
  const [isSavingMemory, setIsSavingMemory] = useState(false);
  const [sidebarQuery, setSidebarQuery] = useState("");
  // pendingAttachments holds files the user has picked but not yet sent.
  // We upload them to the server on submit, get back metadata with a
  // server-trusted path, and forward that in the /api/chat body. The
  // backing per-conv records (pendingAttachmentsByConv / uploadingConvs)
  // are declared up top alongside promptByConv.
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
  // Abort controllers keyed by the conv slot whose POST /chat we're
  // streaming. Multiple chats can be in flight at once; the Stop button
  // (and clearConversation) only aborts the controller for the conv the
  // user is currently looking at. PENDING_CONV_KEY is used until the
  // server promotes the slot to a real id.
  const abortControllersRef = useRef<Record<string, AbortController>>({});
  const fadeTimeoutsRef = useRef<number[]>([]);
  const messagesByConvRef = useRef<Record<string, Message[]>>({});
  const activeConversationIdRef = useRef<string | null>(null);
  // Conv ids this client currently has an SSE socket attached to.
  // PENDING_CONV_KEY = attached to a new chat whose server-side id we
  // haven't heard back yet; otherwise a real conversation id. Multiple
  // entries means we're draining live streams from more than one chat in
  // parallel (which is allowed — the sidebar lights up a dot for each).
  // loadConversation reads this to decide whether to re-fetch from the
  // server (stale partial reply) or trust the local in-memory state
  // (which has whatever the stream has produced so far). The
  // visibilitychange/online effect reads it to decide whether a
  // reattach is needed — if we're attached, the live socket is already
  // keeping state fresh; if we're not but the server says a turn is
  // in-flight, we open GET /stream with Last-Event-ID and resume.
  const attachedConvIdsRef = useRef<Set<string>>(new Set<string>());
  // Cache-bust drift flag. Set when /api/version reports a new build id.
  // We never reload the page automatically — instead we surface an
  // "Update available" button in the sidebar so the user chooses when
  // to refresh. State (not a ref) so the sidebar re-renders on change.
  const [updateAvailable, setUpdateAvailable] = useState(false);
  // Per-conversation last applied SSE event id. Updated whenever the
  // dispatch loop commits an event. On reattach we send this value as
  // Last-Event-ID so the server replays everything AFTER it and we
  // pick up without duplicating already-applied state. The idempotency
  // guard below drops any event whose id is already ≤ this number, so
  // the replay slice that overlaps what we already applied is a no-op.
  //
  // Event IDs are monotonic WITHIN A TURN (start at 1, grow to N) but
  // reset between turns. We also track the current turn_id per conv so
  // we can reset lastEventId when a new turn begins — otherwise a
  // fresh turn's id=1 would be silently dropped as "≤ the previous
  // turn's final id" and the client would hang on a blank reply.
  const lastEventIdByConvRef = useRef<Record<string, number>>({});
  const currentTurnIdByConvRef = useRef<Record<string, string>>({});
  // Guard for concurrent reattach attempts per conv. Without it, two
  // rapid visibilitychange events (unlock + focus) would open two
  // /stream sockets and render every event twice.
  const reattachInFlightRef = useRef<Set<string>>(new Set());

  useEffect(() => {
    messagesByConvRef.current = messagesByConv;
  }, [messagesByConv]);

  useEffect(() => {
    activeConversationIdRef.current = activeConversationId;
  }, [activeConversationId]);

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

  // Theme boot
  useEffect(() => {
    const root = document.documentElement;
    const storageKey = "chat-theme-preference";
    const media = window.matchMedia("(prefers-color-scheme: dark)");

    const resolveTheme = () => {
      const stored = window.localStorage.getItem(storageKey);
      if (stored === "light" || stored === "dark") {
        return stored;
      }
      return media.matches ? "dark" : "light";
    };

    const applyTheme = (nextTheme: "light" | "dark") => {
      root.setAttribute("data-theme", nextTheme);
      setTheme(nextTheme);
    };

    applyTheme(resolveTheme());

    const handleSystemChange = () => {
      const stored = window.localStorage.getItem(storageKey);
      if (stored === "light" || stored === "dark") {
        return;
      }
      applyTheme(media.matches ? "dark" : "light");
    };

    media.addEventListener("change", handleSystemChange);
    return () => media.removeEventListener("change", handleSystemChange);
  }, []);

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

  // Textarea autosize
  useEffect(() => {
    const textarea = promptRef.current;
    if (!textarea) return;
    textarea.style.height = "auto";
    textarea.style.height = `${Math.min(textarea.scrollHeight, 208)}px`;
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
    const handleKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setSidebarOpen(false);
      // Cmd+K (macOS) / Ctrl+K (everywhere else): focus conversation search.
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setSidebarOpen(true);
        // next tick so the sidebar finishes its transform before we focus
        window.setTimeout(() => searchRef.current?.focus(), 80);
      }
    };
    window.addEventListener("keydown", handleKey);
    return () => window.removeEventListener("keydown", handleKey);
  }, []);

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
  }, []);

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

  const filteredConversations = useMemo(() => {
    const q = sidebarQuery.trim().toLowerCase();
    if (!q) return conversations;
    return conversations.filter((c) => c.title.toLowerCase().includes(q));
  }, [conversations, sidebarQuery]);

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
    setMemoryDraft("");
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
        body: JSON.stringify({ content }),
      });
      if (!response.ok) throw new Error(await response.text());
      setMemoryDraft("");
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
    const server = conversations.find((c) => c.id === activeConversationId);
    const name = server?.title?.trim();
    document.title = name ? `${name} — ${base}` : base;
  }, [activeConversationId, conversations, branding.app_name]);

  // Prefer the server's (possibly auto-summarized) title for the active
  // conversation. Falls back to a first-user-message derivation for brand
  // new / unsaved chats.
  const title = useMemo(() => {
    if (activeConversationId) {
      const server = conversations.find((c) => c.id === activeConversationId);
      if (server?.title) return server.title;
    }
    return deriveConversationTitle(messages);
  }, [activeConversationId, conversations, messages]);
  const themeLabel = useMemo(
    () => (theme === "dark" ? "Switch to light mode" : "Switch to dark mode"),
    [theme],
  );
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
      const response = await fetch("/api/conversations", { cache: "no-store" });
      if (!response.ok) return;
      const data = (await response.json()) as { conversations: ConversationSummary[] | null };
      setConversations(data.conversations ?? []);
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
        setSelectedRuntime(conv.runtime || "");
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
        }>;
        pending_memory_proposals?: Array<{
          proposal_id: string;
          content: string;
        }>;
      };
      setActiveConversationId(data.conversation.id);
      setSelectedPersona(data.conversation.persona);
      setSelectedModel(data.conversation.model || DEFAULT_MODEL);
      setSelectedRuntime(data.conversation.runtime || "");
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
        }));
        const memoryCards: MemoryProposal[] = pendingMemoryProposals.map((p) => ({
          id: p.proposal_id,
          content: p.content,
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

  const deleteConversationById = async (conversationId: string) => {
    const response = await fetch(`/api/conversations/${conversationId}`, { method: "DELETE" });
    if (!response.ok) throw new Error("Unable to delete conversation.");
    const remaining = conversations.filter((c) => c.id !== conversationId);
    setConversations(remaining);
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

  // selectRuntime updates the active conversation's runtime flavor. It updates
  // local state immediately (so the next turn's chat body carries it) and, when
  // a conversation already exists, persists it via the dedicated endpoint so a
  // mid-conversation change sticks even without sending a turn. A brand-new chat
  // has no row yet — the choice rides the first chat request body instead.
  const selectRuntime = (name: string) => {
    setSelectedRuntime(name);
    const convId = activeConversationIdRef.current;
    if (!convId) return;
    void fetch(`/api/conversations/${encodeURIComponent(convId)}/runtime`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ runtime: name }),
    }).catch(() => {
      // Best-effort: the flavor still rides the next chat request body, which
      // persists it server-side, so a transient failure here is non-fatal.
    });
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
    // New chat starts on the bundle default flavor (empty = default).
    setSelectedRuntime("");
    promptRef.current?.focus();
  };

  // ── the streaming event loop ─────────────────────────────────────────
  //
  // Consumes chat-server's SSE event names:
  //   conversation           — emitted once with { id, title, persona }
  //   reasoning.start/delta/end
  //   text.delta
  //   tool.call              — { id, name, input }
  //   tool.result            — { id, name, text, is_err }
  //   turn.completed         — { cost_usd, ... }
  //   turn.error             — { message }

  // applyStreamEvent is the per-event dispatch body. It's the single
  // source of truth for how SSE events mutate UI state, shared between
  // the initial POST /chat stream and the GET /stream reattach path
  // (see reattachToConv below). Mutates ctx.target / ctx.hasStartedStreaming
  // in place when an event requires it — the caller threads the same ctx
  // across the whole stream so the mutation is observed by subsequent
  // events in the same loop.
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
        // streaming-set membership the sidebar reads) all point at the
        // same slot the SSE events are now writing to.
        if (attachedConvIdsRef.current.has(oldTarget)) {
          attachedConvIdsRef.current.delete(oldTarget);
          attachedConvIdsRef.current.add(p.id);
        }
        const pendingController = abortControllersRef.current[oldTarget];
        if (pendingController) {
          delete abortControllersRef.current[oldTarget];
          abortControllersRef.current[p.id] = pendingController;
        }
        renameStreamingKey(oldTarget, p.id);
        // Composer state for the per-submission key (rare but possible
        // if the user typed something in the pending view) follows the
        // slot to the real id so a future submit on this conv finds
        // its draft. Use functional setters so the read sees the latest
        // committed value, not the (potentially stale) closure capture.
        setPromptByConv((prev) => {
          const v = prev[oldTarget];
          if (typeof v !== "string") return prev;
          const out = { ...prev };
          delete out[oldTarget];
          if (v !== "") out[p.id] = v;
          return out;
        });
        setPendingAttachmentsByConv((prev) => {
          const v = prev[oldTarget];
          if (!v || v.length === 0) return prev;
          const out = { ...prev };
          delete out[oldTarget];
          out[p.id] = v;
          return out;
        });
        setAttachmentErrorByConv((prev) => {
          const v = prev[oldTarget];
          if (typeof v !== "string") return prev;
          const out = { ...prev };
          delete out[oldTarget];
          out[p.id] = v;
          return out;
        });
        if (uploadingConvsRef.current.has(oldTarget)) {
          uploadingConvsRef.current.delete(oldTarget);
          uploadingConvsRef.current.add(p.id);
          setUploadingConvs(new Set(uploadingConvsRef.current));
        }
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
      const p = payload as { approval_id: string; tool: string; summary: Approval["summary"] };
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
          { id: p.approval_id, tool: p.tool, summary: p.summary, status: "pending" },
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

    // An EXTERNAL ACP agent (Claude Code / Goose) asked for permission to do
    // something sensitive. Surface it inline as an allow/deny prompt; the
    // agent's turn is BLOCKED server-side until the user decides (or the
    // default-deny timeout fires). There is no "approve all".
    if (event.event === "permission.requested") {
      const p = payload as {
        request_id: string;
        title?: string;
        kind?: string;
        locations?: string[];
        options?: PermissionOption[];
      };
      patchAssistantMessage(ctx.target, ctx.assistantId, (m) => {
        const existing = m.permissionRequests ?? [];
        if (existing.some((pr) => pr.id === p.request_id)) return m;
        return {
          ...m,
          permissionRequests: [
            ...existing,
            {
              id: p.request_id,
              title: p.title ?? "The agent wants to perform an action",
              kind: p.kind,
              locations: p.locations,
              options: p.options ?? [],
              status: "pending" as PermissionStatus,
            },
          ],
        };
      });
      return;
    }

    // The server resolved a permission request (the human decided, or the
    // default-deny timeout fired). Reflect the terminal state so a stale prompt
    // can't linger — important when the timeout, not the user, decided.
    if (event.event === "permission.resolved") {
      const p = payload as { request_id: string; allowed?: boolean };
      const status: PermissionStatus = p.allowed ? "allowed" : "denied";
      setMessagesByConv((prev) => {
        const existing = prev[ctx.target];
        if (!existing) return prev;
        const next = existing.map((msg) => {
          if (!msg.permissionRequests?.length) return msg;
          return {
            ...msg,
            permissionRequests: msg.permissionRequests.map((pr) =>
              pr.id === p.request_id && pr.status === "pending" ? { ...pr, status } : pr,
            ),
          };
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

  // pumpStreamResponse reads an SSE Response body, parses frames,
  // applies the idempotency guard (dropping events whose id ≤ the last
  // one we applied for this conv), and dispatches through
  // applyStreamEvent. Shared by the POST /api/chat initial stream and
  // the GET /api/conversations/{id}/stream reattach path.
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

        // Turn boundary detection. SSE event IDs are monotonic WITHIN
        // a turn but reset to 1 for each new turn — so we must clear
        // the idempotency guard every time the server reports a
        // different turn_id for this conv. Without this, a fresh
        // turn's id=1 gets dropped as "already applied" against the
        // prior turn's final id.
        if (event.event === "turn.started") {
          const p = payload as { turn_id?: string };
          if (p.turn_id && currentTurnIdByConvRef.current[ctx.target] !== p.turn_id) {
            currentTurnIdByConvRef.current[ctx.target] = p.turn_id;
            lastEventIdByConvRef.current[ctx.target] = 0;
          }
        }

        // Idempotency guard — drops any event id we've already applied
        // for this conv + turn. Critical on reattach: the server's
        // replay slice overlaps what we saw on the original POST
        // stream until the disconnect point.
        if (event.id !== undefined) {
          const eid = Number(event.id);
          if (Number.isFinite(eid)) {
            const last = lastEventIdByConvRef.current[ctx.target] ?? 0;
            if (eid <= last) continue;
            lastEventIdByConvRef.current[ctx.target] = eid;
          }
        }

        await applyStreamEvent(event, payload, ctx);
      }
    }
  };

  // reattachToConv opens GET /stream for the given conv if chat-server
  // reports an in-flight (or recently-finished, retained) turn.
  // Replays from Last-Event-ID, then streams live events until the
  // buffer seals. Reuses the existing assistant message slot when one
  // is mid-turn (`thinking` or `streaming` — phone lock/unlock or fresh
  // send that backgrounded before any text arrived); otherwise creates
  // a new one so a freshly-refreshed page reconstructs the full turn
  // from replay. The fresh slot starts in `thinking` so the indicator
  // shows until text arrives — line 1648's text-event patch flips it
  // to `streaming` once content actually starts.
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
  }, []);

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

  const toggleTheme = () => {
    const nextTheme = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", nextTheme);
    window.localStorage.setItem("chat-theme-preference", nextTheme);
    setTheme(nextTheme);
  };

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

  /**
   * Regenerate the last assistant response. Wired to a "Regenerate" button
   * on the final assistant message. Same plumbing as retryLastUserMessage
   * but distinguishes intent in the UI (regenerate after a successful turn).
   */
  const regenerateLastAssistant = async () => {
    await retryLastUserMessage();
  };

  /**
   * resendUserMessage edits a prior user turn and re-runs from that point.
   * Trims every message after the edited user bubble (UI + server), then
   * submits the new text. Only the MOST RECENT user message is editable —
   * editing anything older would require surgical history rewriting that
   * muddles the model's sense of what "happened".
   */
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

  /**
   * retryLastUserMessage re-runs the most recent user turn. Used by the
   * "Retry" affordance that appears on cancelled or failed assistant
   * messages. It drops the trailing failed/cancelled assistant message
   * client-side (and asks the server to truncate too) so the retry
   * re-requests from the same point in the transcript.
   */
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

  // uploadPendingAttachments POSTs every queued file to /api/attachments in
  // one multipart request and returns the server-validated metadata. It
  // also mirrors per-file status into pendingAttachments so the chips can
  // render an in-flight / errored state if we ever add per-file UI later.
  const uploadPendingAttachments = async (
    composerKey: string,
  ): Promise<UploadedAttachmentMeta[]> => {
    const files = pendingAttachmentsByConv[composerKey] ?? [];
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
    if ((pendingAttachmentsByConv[composerKey] ?? []).length > 0) {
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
    if (selectedRuntime) {
      body.runtime = selectedRuntime;
    }
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
        <aside
          className={[
            // Mobile: 85vw-capped drawer that leaves a thumb-sized strip on
            // the right so users can swipe-dismiss or see they're in an
            // overlay. safe-area-inset-left so it doesn't slide under an
            // iPhone notch / rounded corner.
            "fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(19rem,85vw)] flex-col overflow-auto border-r border-[var(--color-border)] bg-[color-mix(in_srgb,var(--sidebar-surface)_96%,black)] px-3 py-4 shadow-[var(--shadow-lg)] backdrop-blur-xl transition-transform duration-200 sm:w-[min(17rem,calc(100vw-2.5rem))] sm:bg-[var(--sidebar-surface)] lg:sticky lg:h-screen lg:w-auto lg:translate-x-0 lg:border-r-0 lg:bg-[var(--sidebar-surface)] lg:shadow-none lg:backdrop-blur-0",
            sidebarOpen ? "translate-x-0" : "-translate-x-full",
          ].join(" ")}
          style={{
            paddingLeft: "max(0.75rem, env(safe-area-inset-left))",
            paddingBottom: "max(1rem, env(safe-area-inset-bottom))",
          }}
        >
          <div className="mb-4 flex items-center justify-between px-1">
            <a className="flex items-center gap-2.5 no-underline" href="#">
              <Image
                src="/logos/elcano-mark-primary.svg"
                alt={branding.app_name}
                width={28}
                height={28}
                priority
              />
              <span className="font-heading text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">
                {branding.app_name}
              </span>
            </a>
            <div className="flex items-center gap-1">
              {/* Three modes:
                  - lockdownOnly: only the lockdown +button (every
                    chat is forcibly lockdown).
                  - lockdownAvailable && !lockdownOnly: both buttons.
                  - !lockdownAvailable: only the regular +button (no
                    sandbox image configured, lockdown unsupported). */}
              {!serverConfig.lockdownOnly ? (
                <button
                  className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7"
                  type="button"
                  title="New chat"
                  aria-label="New chat"
                  onClick={() => clearConversation()}
                >
                  <Icon name="plus" className="size-4" />
                </button>
              ) : null}
              {serverConfig.lockdownAvailable ? (
                <button
                  className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-accent)] sm:size-7"
                  type="button"
                  title={
                    serverConfig.lockdownOnly
                      ? "New chat — every chat on this server is sealed (sandboxed, vetted model only)"
                      : "New sealed chat — sandboxed, vetted model, nothing leaves"
                  }
                  aria-label={
                    serverConfig.lockdownOnly
                      ? "New chat — every chat on this server is sealed (sandboxed, vetted model only)"
                      : "New sealed chat — sandboxed, vetted model, nothing leaves"
                  }
                  onClick={() => clearConversation({ lockdown: true })}
                >
                  <Icon name="lock" className="size-4" />
                </button>
              ) : null}
              <button
                aria-label="Close sidebar"
                className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7 lg:hidden"
                type="button"
                onClick={() => setSidebarOpen(false)}
              >
                <svg
                  aria-hidden="true"
                  className="size-4"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.9"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                >
                  <path d="M18 6 6 18" />
                  <path d="m6 6 12 12" />
                </svg>
              </button>
            </div>
          </div>

          <div className="mb-2 px-2 text-[0.75rem] text-[var(--color-text-muted)]">
            {userEmail || "Loading..."}
          </div>

          {/* Cross-view link to the orchestrator (View B). The one middleware
              gates both /chat and /orchestrator off the same session, so this
              navigates without a re-login. */}
          <a
            href="/orchestrator"
            data-testid="nav-to-orchestrator"
            className="mb-4 block rounded-md px-2 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
          >
            Operations Center →
          </a>

          <div className="flex-1 overflow-y-auto">
            <p className="mb-1 px-2 text-[0.6875rem] font-medium text-[var(--color-text-muted)]">
              Conversations
            </p>
            <div className="mb-1 px-2">
              <input
                ref={searchRef}
                type="search"
                value={sidebarQuery}
                onChange={(e) => setSidebarQuery(e.target.value)}
                placeholder={`Search chats (${searchShortcut})`}
                className="w-full rounded-md border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
              />
            </div>

            {isLoadingHistory ? (
              <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">Loading...</p>
            ) : filteredConversations.length === 0 ? (
              <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">
                {sidebarQuery.trim() ? "No matches." : "No saved chats yet."}
              </p>
            ) : (
              filteredConversations.map((conversation) => (
                <div
                  key={conversation.id}
                  className={[
                    "group relative rounded-md transition",
                    activeConversationId === conversation.id
                      ? "bg-[var(--color-overlay-soft)]"
                      : "hover:bg-[var(--color-overlay-soft)]",
                  ].join(" ")}
                >
                  <button
                    className={[
                      "block w-full min-w-0 rounded-md px-2 py-1.5 pr-36 text-left text-[0.8125rem] transition sm:pr-[6.25rem]",
                      activeConversationId === conversation.id
                        ? "text-[var(--color-text-primary)]"
                        : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
                    ].join(" ")}
                    type="button"
                    onClick={() => void loadConversation(conversation.id)}
                  >
                    <span className="flex min-w-0 items-center gap-1.5">
                      {streamingConvs.has(conversation.id) ? (
                        <span
                          aria-label="Working"
                          title="Working…"
                          className="inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--color-accent)]"
                        />
                      ) : null}
                      {conversation.lockdown ? (
                        <Icon
                          name="lock"
                          className="size-3 shrink-0 text-[var(--color-accent)]"
                        />
                      ) : null}
                      <span className="block truncate">{conversation.title}</span>
                    </span>
                  </button>

                  <div className="touch-reveal pointer-events-none absolute inset-y-0 right-1 flex items-center gap-1 opacity-0 transition group-hover:opacity-100 group-focus-within:opacity-100">
                    <button
                      aria-label={`Download ${conversation.title} as JSON`}
                      title="Download as JSON"
                      className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                      type="button"
                      onClick={() => void downloadConversation(conversation)}
                    >
                      <Icon name="download" className="size-3.5" />
                    </button>
                    <button
                      aria-label={`Delete ${conversation.title}`}
                      className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                      type="button"
                      onClick={() =>
                        setPendingDeleteConversation({ id: conversation.id, title: conversation.title })
                      }
                    >
                      <Icon name="trash" className="size-3.5" />
                    </button>
                    <button
                      aria-label={conversation.pinned ? `Unpin ${conversation.title}` : `Pin ${conversation.title}`}
                      title={conversation.pinned ? "Unpin" : "Pin"}
                      className={[
                        "pointer-events-auto inline-flex size-10 items-center justify-center rounded-md transition hover:bg-[var(--color-overlay-strong)] sm:size-7",
                        conversation.pinned
                          ? "text-[var(--color-accent)]"
                          : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]",
                      ].join(" ")}
                      type="button"
                      onClick={() => void togglePin(conversation)}
                    >
                      <svg
                        aria-hidden="true"
                        viewBox="0 0 24 24"
                        className="size-3.5"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth={conversation.pinned ? 2.2 : 1.8}
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      >
                        <path d="M12 17v5" />
                        <path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z" />
                      </svg>
                    </button>
                  </div>
                </div>
              ))
            )}
          </div>

          <div className="grid gap-1 pt-3">
            {updateAvailable ? (
              <button
                type="button"
                className="flex w-full items-center gap-2 rounded-md border border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 px-2 py-1.5 text-left text-[0.75rem] font-medium text-[var(--color-accent)] transition hover:bg-[var(--color-accent)]/20 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                onClick={() => window.location.reload()}
                title="A newer version of the app has been deployed. Click to refresh and load it."
              >
                <Icon name="refresh" className="size-3.5 shrink-0" />
                Chat has been updated — click to refresh
              </button>
            ) : null}
            {conversations.some((c) => !c.pinned) ? (
              <button
                type="button"
                className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.75rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-danger)]"
                onClick={() => setConfirmBulkDelete(true)}
                title="Delete every unpinned conversation"
              >
                <Icon name="trash" className="size-3.5 shrink-0" />
                Delete all unpinned
              </button>
            ) : null}
            <form action="/api/auth/logout" method="post">
              <button
                className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                type="submit"
              >
                <Icon name="logout" className="size-4 shrink-0" />
                Sign out
              </button>
            </form>
          </div>
        </aside>

        <button
          aria-label="Close sidebar"
          className={[
            "fixed inset-0 z-20 bg-[color-mix(in_srgb,var(--color-overlay-strong)_120%,black)] backdrop-blur-[2px] transition lg:hidden",
            sidebarOpen ? "block" : "hidden",
          ].join(" ")}
          type="button"
          onClick={() => setSidebarOpen(false)}
        />

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

              <div className="grid gap-2">
                <textarea
                  className="min-h-24 w-full resize-y rounded-[0.9rem] border border-[var(--color-border-strong)] bg-transparent px-3 py-2 text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
                  placeholder="Remember that deal names may contain intentional typos."
                  value={memoryDraft}
                  onChange={(event) => setMemoryDraft(event.target.value)}
                />
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <p className="text-[0.72rem] text-[var(--color-text-muted)]">
                    Chat also auto-saves explicit phrases like “remember this:” or “memorize that:”.
                  </p>
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
                    {memories.map((memory) => (
                      <div
                        key={memory.id}
                        className="rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3"
                      >
                        <p className="whitespace-pre-wrap text-[0.875rem] leading-[1.5] text-[var(--color-text-primary)]">
                          {memory.content}
                        </p>
                        <div className="mt-2 flex flex-wrap items-center justify-between gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
                          <span>
                            {memory.source === "chat"
                              ? "Saved from chat"
                              : memory.source === "proposed"
                                ? "Proposed"
                                : "Manual"}
                          </span>
                          <div className="flex items-center gap-3">
                            <button
                              type="button"
                              className="hover:text-[var(--color-text-primary)]"
                              onClick={() => {
                                setEditingMemoryId(memory.id);
                                setMemoryDraft(memory.content);
                              }}
                            >
                              Edit
                            </button>
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
                    ))}
                  </div>
                )}
              </div>
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

        <main
          // grid-cols-[minmax(0,1fr)] is the load-bearing class here. Without
          // an explicit column, the default `grid-auto-columns: auto` sizes
          // the track to max-content of the children — and because main has
          // `overflow: hidden` (scroll container), the track is free to grow
          // past main's own width. On a 375px viewport that meant the header,
          // conversation section, and composer all laid out at ~413px and
          // visibly bled past the right edge when a history with rich content
          // (user bubble with a long prompt, composer with Persona + Model
          // + Send side-by-side) loaded. Pinning to minmax(0, 1fr) clamps the
          // track to the container width; min-w-0 on descendants still lets
          // wide children scroll internally.
          className="grid min-h-0 min-w-0 grid-cols-[minmax(0,1fr)] grid-rows-[auto_minmax(0,1fr)_auto] gap-2 overflow-hidden px-3 pt-[max(0.75rem,env(safe-area-inset-top))] pb-3 sm:gap-3 sm:px-6 sm:pt-[max(1.25rem,env(safe-area-inset-top))] sm:pb-5 lg:px-8 xl:px-10"
          suppressHydrationWarning
        >
          <header className="flex items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-3">
              <button
                aria-label="Open sidebar"
                className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none sm:size-8 lg:hidden"
                type="button"
                onClick={() => setSidebarOpen(true)}
              >
                <svg
                  aria-hidden="true"
                  className="size-4"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.9"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                >
                  <path d="M4 6h16" />
                  <path d="M4 12h16" />
                  <path d="M4 18h16" />
                </svg>
              </button>

              {renamingTitleDraft !== null && activeConversationId ? (
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
                  className="min-w-0 flex-1 rounded-md border border-[var(--color-accent)] bg-transparent px-1.5 py-0.5 text-[0.8125rem] font-medium text-[var(--color-text-primary)] outline-none sm:text-[0.9375rem]"
                />
              ) : (
                <button
                  type="button"
                  disabled={!activeConversationId}
                  title={activeConversationId ? "Click to rename" : undefined}
                  onClick={() => {
                    if (!activeConversationId) return;
                    setRenamingTitleDraft(title);
                  }}
                  className="min-w-0 truncate rounded-md px-1.5 py-0.5 text-left text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition enabled:cursor-text enabled:hover:bg-[var(--color-overlay-soft)] enabled:hover:text-[var(--color-text-primary)] disabled:cursor-default sm:text-[0.9375rem]"
                >
                  {title}
                </button>
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

            <div className="inline-flex items-center gap-1">
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
              <button
                aria-label={themeLabel}
                aria-pressed={theme === "dark"}
                className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8"
                title="Toggle theme"
                type="button"
                onClick={toggleTheme}
              >
                <span className="relative size-4" aria-hidden="true">
                  <Icon
                    name="sun"
                    className={[
                      "absolute inset-0 size-4 transition duration-200",
                      theme === "light"
                        ? "rotate-0 scale-100 opacity-100"
                        : "-rotate-12 scale-[0.86] opacity-0",
                    ].join(" ")}
                  />
                  <Icon
                    name="moon"
                    className={[
                      "absolute inset-0 size-4 transition duration-200",
                      theme === "dark"
                        ? "rotate-0 scale-100 opacity-100"
                        : "rotate-12 scale-[0.86] opacity-0",
                    ].join(" ")}
                  />
                </span>
              </button>
            </div>
          </header>

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
            <div className="mx-auto grid min-h-full w-full min-w-0 max-w-[52rem] content-start gap-3 pb-4 sm:gap-4 sm:pb-6">
              {isLoadingHistory ? (
                <div className="flex min-h-full items-center justify-center pb-8 text-[0.875rem] text-[var(--color-text-muted)]">
                  Loading conversation...
                </div>
              ) : messages.length === 0 ? (
                <div className="flex min-h-full flex-col items-center justify-center gap-6 pb-6 sm:gap-8 sm:pb-8">
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
                <div className="grid min-w-0 gap-5 sm:gap-6">
                  {isSummarizing ? (
                    <SummarizeProgressCard
                      startedAt={summarizeStartedAt}
                      streamingText={summarizeStream}
                    />
                  ) : null}
                  {summarizeError ? (
                    <div
                      role="alert"
                      className="rounded-[0.6rem] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.78rem] text-[var(--color-danger,#dc2626)]"
                    >
                      {summarizeError}
                    </div>
                  ) : null}
                  {messages.map((message, idx) => {
                    // Compaction boundary: when a summary message
                    // exists, every message before it is collapsed by
                    // default behind a single expander row. We render
                    // the expander at the position of the first
                    // hidden message and short-circuit the others.
                    if (summaryIndex >= 0 && idx < summaryIndex) {
                      if (!summaryExpanded) {
                        if (idx !== 0) return null;
                        return (
                          <button
                            key="__collapsed_range_expander__"
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
                    }
                    if (message.kind === "summary") {
                      return (
                        <SummaryBanner
                          key={message.id}
                          message={message}
                          collapsedRangeCount={summaryIndex}
                          summaryExpanded={summaryExpanded}
                          onToggleExpand={() => setSummaryExpanded((v) => !v)}
                          onResummarize={() => setConfirmSummarize(true)}
                          isSummarizing={isSummarizing}
                        />
                      );
                    }
                    const isPreSummary = summaryIndex >= 0 && idx < summaryIndex;
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
                        key={message.id}
                        className={[
                          // min-w-0 lets this grid item shrink to its track
                          // instead of expanding to fit a wide descendant
                          // (long URL in a user bubble, long stdout line in
                          // a tool result). Without it the item's default
                          // min-width:auto pushes the whole chat column
                          // past the viewport on mobile.
                          "flex min-w-0 gap-3",
                          message.role === "user"
                            ? "justify-end message-enter-user"
                            : "justify-start message-enter-assistant",
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
                                        <ToolChip key={tc.id} tc={tc} taskTrackerDisplay={taskTrackerDisplay} />
                                      ))}
                                    </div>
                                  ) : null}
                                  {pythonStreams.length > 0 ? (
                                    <div className="grid gap-1.5">
                                      {pythonStreams.map((stream, i) => (
                                        <PythonOutput key={i} stream={stream} />
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
                                    "flex min-w-0 items-center gap-2 text-[0.875rem] text-[var(--color-text-muted)] transition-opacity duration-200 ease-out",
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

                              {renderAssistantContent(
                                message.content,
                                message.state === "streaming" || message.state === "thinking",
                                realConvId(currentConvKey),
                              )}
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

                              {message.permissionRequests && message.permissionRequests.length > 0 ? (
                                <div className="grid gap-1.5">
                                  {message.permissionRequests.map((pr) => (
                                    <PermissionCard
                                      key={pr.id}
                                      request={pr}
                                      conversationId={realConvId(currentConvKey) ?? ""}
                                      onResolved={(next) => {
                                        patchAssistantMessage(currentConvKey, message.id, (m) => ({
                                          ...m,
                                          permissionRequests: (m.permissionRequests ?? []).map((x) =>
                                            x.id === next.id ? next : x,
                                          ),
                                        }));
                                      }}
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
                                </div>
                              ) : null}
                            </div>
                          </div>
                          )}
                        </div>
                      </article>
                    );
                  })}
                  <div ref={streamEndRef} />
                </div>
              )}
            </div>
          </section>

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
                className="mx-auto mb-1 w-full max-w-[52rem] rounded-[0.9rem] border border-[var(--color-danger,#dc2626)] bg-[color-mix(in_srgb,var(--color-danger,#dc2626)_10%,transparent)] px-3 py-2 text-[0.75rem] text-[var(--color-danger,#dc2626)] sm:mb-1.5"
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
            <form
              className={`relative mx-auto w-full max-w-[52rem] rounded-[1.2rem] border bg-[var(--composer-surface)] px-3 pt-3 pb-2.5 shadow-[var(--composer-shadow)] sm:rounded-[1.75rem] sm:px-4 sm:pt-4 sm:pb-3 transition-colors ${isDraggingOver ? "border-[var(--color-accent)] ring-2 ring-[var(--color-accent)]/30" : "border-[var(--color-border)]"}`}
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
                <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center rounded-[1.2rem] sm:rounded-[1.75rem] bg-[var(--color-accent)]/10">
                  <span className="text-[0.8rem] font-medium text-[var(--color-accent)]">Drop to attach</span>
                </div>
              )}
              <label className="sr-only" htmlFor="promptInput">
                Message
              </label>
              <textarea
                id="promptInput"
                ref={promptRef}
                className="min-h-[2.6rem] max-h-[10rem] w-full resize-none bg-transparent px-0 pt-0 pb-2 text-[16px] leading-[1.45] text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-muted)] sm:min-h-[3rem] sm:max-h-[13rem] sm:pb-3 sm:text-[var(--font-size-body)]"
                placeholder={promptPlaceholder}
                rows={1}
                suppressHydrationWarning
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" && !event.shiftKey) {
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
                  }
                }}
              />

              {pendingAttachments.length > 0 || attachmentError ? (
                <div className="mb-2 flex flex-wrap items-center gap-1.5">
                  {pendingAttachments.map((a) => (
                    <PendingAttachmentChip
                      key={a.clientId}
                      attachment={a}
                      onRemove={() => removePendingAttachment(a.clientId)}
                      removalDisabled={isStreaming || isUploadingAttachments}
                    />
                  ))}
                  {attachmentError ? (
                    <span className="text-[0.7rem] text-[var(--color-danger,#dc2626)]">
                      {attachmentError}
                    </span>
                  ) : null}
                </div>
              ) : null}

              {spreadsheetNudge.show ? (
                <div
                  role="status"
                  className="mb-2 flex flex-wrap items-center justify-between gap-2 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)]"
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

              <div className="flex items-end justify-between gap-2">
                <div className="flex min-w-0 flex-wrap items-center gap-2 overflow-visible">
                  <button
                    type="button"
                    aria-label="Attach files"
                    title="Attach files"
                    className="inline-flex size-7 shrink-0 items-center justify-center rounded-full border border-[var(--color-border-strong)] text-[var(--color-text-secondary)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
                    disabled={isStreaming || isUploadingAttachments}
                    onClick={() => fileInputRef.current?.click()}
                  >
                    <Icon name="paperclip" className="size-3.5" />
                  </button>
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
                        className="relative inline-flex min-w-0 items-center gap-1.5 text-[0.72rem] text-[var(--color-text-muted)]"
                      >
                        <button
                          type="button"
                          aria-haspopup="listbox"
                          aria-expanded={personaPickerOpen}
                          title={`Persona — ${formatPersona(selectedPersona)}`}
                          // Collapsed default = circle the same size as
                          // the paperclip / wrench buttons (h-7 w-7) so
                          // the composer toolbar reads as a row of
                          // matching controls instead of two long pills
                          // hogging space. It stays that size until you
                          // click it open — no hover/focus expansion, so
                          // the click target never moves out from under
                          // the cursor. Open reveals the persona name and
                          // the dropdown marks the active one.
                          className={`composer-pill-text group relative inline-flex h-7 shrink-0 items-center justify-center overflow-hidden rounded-full border border-[var(--color-border-strong)] bg-transparent text-[0.72rem] text-[var(--color-text-secondary)] transition-[width] duration-200 hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] ${
                            personaPickerOpen ? "w-24 sm:w-44" : "w-7"
                          }`}
                          onClick={() => setPersonaPickerOpen((open) => !open)}
                        >
                          <span
                            aria-hidden="true"
                            className={`absolute inset-0 grid place-items-center transition-opacity duration-200 ${
                              personaPickerOpen ? "opacity-0" : "opacity-100"
                            }`}
                          >
                            <Icon name="persona" className="size-3.5" />
                          </span>
                          <span
                            className={`truncate px-2.5 transition-opacity duration-200 ${
                              personaPickerOpen ? "opacity-100" : "opacity-0"
                            }`}
                          >
                            {formatPersona(selectedPersona)}
                          </span>
                        </button>
                        {personaPickerOpen ? (
                          <div
                            role="listbox"
                            aria-label="Persona"
                            className="absolute bottom-[calc(100%+0.35rem)] left-0 z-30 w-40 overflow-hidden rounded-[0.9rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-surface-2)_96%,black)] shadow-[var(--shadow-lg)] backdrop-blur-xl sm:w-44"
                          >
                            <div className="max-h-72 overflow-y-auto py-1">
                              {personaOptions.map((p) => {
                                const selected = p === selectedPersona;
                                return (
                                  <button
                                    key={p}
                                    type="button"
                                    role="option"
                                    aria-selected={selected}
                                    className={[
                                      "flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-[0.74rem] transition hover:bg-[var(--color-overlay-soft)]",
                                      selected
                                        ? "bg-[var(--color-overlay-soft)] font-semibold text-[var(--color-accent)]"
                                        : "text-[var(--color-text-primary)]",
                                    ].join(" ")}
                                    onMouseDown={(event) => event.preventDefault()}
                                    onClick={() => {
                                      setSelectedPersona(p);
                                      setPersonaPickerOpen(false);
                                    }}
                                  >
                                    <span className="truncate">{formatPersona(p)}</span>
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
                          </div>
                        ) : null}
                      </div>
                    );
                  })()}
                  {/* Runtime flavor picker — self-hides unless the bundle ships
                      more than one flavor. Lets a conversation pick the native
                      in-process fast path vs the sandboxed native-acp agent
                      (and, later, external ACP flavors). */}
                  <RuntimePicker
                    flavors={runtimes}
                    selected={selectedRuntime}
                    defaultRuntime={defaultRuntime}
                    onSelect={selectRuntime}
                  />
                  {/* No leading "Persona" / "Model" word labels — the
                      pill values themselves convey what each control is,
                      and dropping the labels keeps the persona dropdown's
                      `left-0` anchored to the button (with the label, on
                      desktop it offset the dropdown to the left of the
                      pill). Encourages exploration: the pills look
                      clickable on their own. */}
                  {/* Wrapper is a <div>, not a <label>: with <label> a click
                      anywhere inside (e.g. on a model option) bubbles up and
                      triggers the implicit "focus my <input>" behavior, which
                      re-runs the input's onFocus handler and re-opens the
                      picker we just closed. <div> drops that behavior; the
                      input is still focusable directly. */}
                  <div ref={modelPickerRef} className="relative inline-flex min-w-0 items-center gap-1.5 text-[0.72rem] text-[var(--color-text-muted)]">
                    {/* Same collapsed-circle treatment as the persona
                        button — a centered icon stands in for the model in
                        the collapsed state, and the live input fades in only
                        once the picker is open (click/focus). No hover/focus
                        width expansion, so the control isn't a moving target. */}
                    <div
                      className={`group relative h-7 shrink-0 overflow-hidden rounded-full border bg-transparent transition-[width] duration-200 ${
                        modelError
                          ? "border-[var(--color-danger,#dc2626)]"
                          : "border-[var(--color-border-strong)] hover:border-[var(--color-accent)]"
                      } ${
                        isStreaming
                          ? "w-7 cursor-not-allowed opacity-40"
                          : modelPickerOpen
                            ? "w-24 sm:w-44"
                            : "w-7"
                      }`}
                    >
                      <span
                        aria-hidden="true"
                        className={`pointer-events-none absolute inset-0 grid place-items-center transition-opacity duration-200 ${
                          modelError ? "text-[var(--color-danger,#dc2626)]" : "text-[var(--color-text-secondary)]"
                        } ${
                          modelPickerOpen ? "opacity-0" : "opacity-100"
                        }`}
                      >
                        <Icon name="model" className="size-3.5" />
                      </span>
                      <input
                        ref={modelInputRef}
                        type="text"
                        spellCheck={false}
                        autoCapitalize="off"
                        autoCorrect="off"
                        placeholder="default"
                        aria-label="Model"
                        // text-center matches the persona button next to
                        // it (which is flex-centered). Without it the
                        // input sits left-aligned and the two pills read
                        // as different controls even when same-sized.
                        // truncate on the input ellipses long model slugs
                        // when the field isn't focused (modern browsers
                        // honor text-overflow:ellipsis on <input> in that
                        // state) — when the user taps to edit, native
                        // input scroll takes over and the ellipsis lifts.
                        className={`relative h-full w-full truncate bg-transparent px-2.5 text-center text-[0.72rem] outline-none transition-opacity duration-200 disabled:opacity-60 ${
                          modelError
                            ? "text-[var(--color-danger,#dc2626)]"
                            : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]"
                        } ${
                          modelPickerOpen
                            ? "opacity-100"
                            : "opacity-0 focus:opacity-100"
                        }`}
                        value={labelForModel(selectedModel)}
                        disabled={isStreaming}
                        title={`OpenRouter model slug — aliases: ${DEFAULT_MODEL_LABEL} → ${DEFAULT_MODEL}, ${ADVANCED_MODEL_LABEL} → ${ADVANCED_MODEL}`}
                        onFocus={() => {
                          setModelPickerOpen(true);
                          setModelSearchQuery("");
                          void loadRankedModels();
                          void loadCatalogModels();
                        }}
                        onClick={() => {
                          setModelPickerOpen(true);
                          setModelSearchQuery("");
                          void loadRankedModels();
                          void loadCatalogModels();
                        }}
                        onChange={(event) => {
                          setSelectedModel(event.target.value);
                          setModelSearchQuery(event.target.value);
                          setModelPickerOpen(true);
                          void loadRankedModels();
                          void loadCatalogModels();
                        }}
                        onKeyDown={(event) => {
                          if (event.key === "Escape") setModelPickerOpen(false);
                        }}
                      />
                    </div>
                    {modelPickerOpen && !isStreaming ? (
                      // Mobile pins the popover to the viewport so the
                      // anchored layout can't push it off-screen. Desktop
                      // anchors to the picker button's left edge and grows
                      // rightward — the composer's footer cluster lives on
                      // the left of the row, so there's always more room to
                      // the right; right-anchoring used to extend the
                      // popover leftward and got clipped by `main`'s
                      // overflow-hidden (and visually covered by the
                      // sidebar overlay at sm-to-lg widths).
                      <div className="fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 overflow-hidden rounded-[0.9rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-surface-2)_96%,black)] shadow-[var(--shadow-lg)] backdrop-blur-xl sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.35rem)] sm:left-0 sm:w-64">
                        <div className="max-h-72 overflow-y-auto py-1">
                          {isLoadingRankedModels || (isLoadingCatalog && modelSearchQuery.trim() !== "") ? (
                            <div className="px-3 py-2 text-[0.74rem] text-[var(--color-text-muted)]">Loading...</div>
                          ) : filteredRankedModels.length === 0 ? (
                            <div className="px-3 py-2 text-[0.74rem] text-[var(--color-text-muted)]">No matches</div>
                          ) : (
                            filteredRankedModels.map((model) => {
                              // Tier rows get a "recommended" pill so the user
                              // can spot the curated picks at a glance; non-tier
                              // rows show the tested/experimental validation
                              // signal instead.
                              // One pill per row, picked from a strict
                              // hierarchy so the dropdown stays uncluttered:
                              //   recommended > tested > ✨ new > experimental
                              // recommended = tier slug (default/advanced).
                              // tested = vetted end-to-end with our tools.
                              // new = listed on OpenRouter within the last
                              //   NEW_MODEL_WINDOW_DAYS.
                              // experimental = catchall: a known slug we
                              //   haven't classified.
                              const tier = model.slug ? tierForModel(model.slug) : null;
                              const isTier = tier === "default" || tier === "advanced";
                              const isFresh = isNewlyReleased(model.created);
                              let pill: ReactNode = null;
                              if (isTier) {
                                pill = (
                                  <span className="shrink-0 rounded-full bg-[var(--color-accent)] px-1.5 py-0 text-[0.6rem] font-semibold leading-4 tabular-nums text-[var(--color-surface-1)]">
                                    recommended
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
                              return (
                                <button
                                  key={model.slug || "__default__"}
                                  type="button"
                                  aria-current={isSelected ? "true" : undefined}
                                  className={`flex w-full items-center justify-between gap-2 px-3 py-2 text-left text-[0.74rem] transition hover:bg-[var(--color-overlay-soft)] ${
                                    isSelected
                                      ? "bg-[var(--color-overlay-soft)] font-semibold text-[var(--color-accent)]"
                                      : "text-[var(--color-text-primary)]"
                                  }`}
                                  title={model.slug ? `${model.name} (${model.slug})` : "Use the server-configured default model"}
                                  onMouseDown={(event) => event.preventDefault()}
                                  onClick={() => {
                                    setSelectedModel(model.slug);
                                    setModelSearchQuery("");
                                    setModelPickerOpen(false);
                                    // Blur the input so the on-screen
                                    // keyboard dismisses on mobile and
                                    // `focus-within` clears, letting the
                                    // wrapper collapse back to the
                                    // circle. Safe on desktop too —
                                    // matches the pre-collapsing
                                    // behavior where focus naturally
                                    // moved when the picker closed.
                                    modelInputRef.current?.blur();
                                  }}
                                >
                                  <span className="truncate">{model.name}</span>
                                  {pill}
                                </button>
                              );
                            })
                          )}
                        </div>
                      </div>
                    ) : null}
                  </div>
                  {mcpServers.length > 0 ? (
                    <div ref={mcpPickerRef} className="relative inline-flex">
                      {(() => {
                        const enabledCount = mcpServers.filter((s) => s.enabled).length;
                        return (
                          <button
                            type="button"
                            aria-label="Optional tools"
                            disabled={isStreaming}
                            title="Optional tools for this conversation"
                            className={`inline-flex h-7 shrink-0 items-center justify-center gap-1 rounded-full border border-[var(--color-border-strong)] text-[var(--color-text-secondary)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40 ${enabledCount > 0 ? "pl-2 pr-1.5" : "w-7"}`}
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
                            {enabledCount > 0 ? (
                              <span className="inline-flex min-w-[1rem] items-center justify-center rounded-full bg-[var(--color-accent)] px-1 text-[0.6rem] font-medium leading-4 text-[var(--color-surface-1)] tabular-nums">
                                {enabledCount}
                              </span>
                            ) : null}
                          </button>
                        );
                      })()}
                      {mcpPickerOpen && !isStreaming ? (
                        // Same fixed/absolute split as the model picker
                        // above — see the comment there for context.
                        <div className="fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 overflow-hidden rounded-[0.9rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-surface-2)_96%,black)] shadow-[var(--shadow-lg)] backdrop-blur-xl sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.35rem)] sm:left-0 sm:w-72">
                          <div className="max-h-80 overflow-y-auto py-1">
                            {isLoadingMcpServers ? (
                              <div className="px-3 py-2 text-[0.74rem] text-[var(--color-text-muted)]">Loading...</div>
                            ) : (
                              mcpServers.map((server) => (
                                <label
                                  key={server.name}
                                  className="flex cursor-pointer items-start gap-2 px-3 py-2 text-[0.74rem] transition hover:bg-[var(--color-overlay-soft)]"
                                  title={(server.tools ?? []).join(", ")}
                                >
                                  <input
                                    type="checkbox"
                                    className="mt-0.5 h-3.5 w-3.5 shrink-0"
                                    checked={server.enabled}
                                    onChange={() => {
                                      void toggleMcpServer(activeConversationId, server.name);
                                    }}
                                  />
                                  <span className="flex flex-col gap-0.5">
                                    <span className="flex items-center gap-1.5">
                                      <span className="font-medium text-[var(--color-text-primary)]">
                                        {server.display_name || server.name}
                                      </span>
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
                                      <span className="text-[0.7rem] leading-snug text-[var(--color-text-muted)]">
                                        {server.description}
                                      </span>
                                    ) : null}
                                    <span className="text-[0.65rem] text-[var(--color-text-muted)]">
                                      {server.tool_count} tool{server.tool_count === 1 ? "" : "s"}
                                    </span>
                                  </span>
                                </label>
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

                <div className="flex items-center gap-2">
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
                  <button
                    aria-label="Send message"
                    className="inline-flex min-w-[3rem] items-center justify-center rounded-full bg-[var(--color-text-primary)] px-3 py-2 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:cursor-not-allowed disabled:opacity-30 sm:min-w-[3.25rem]"
                    type="submit"
                    disabled={!prompt.trim() || isStreaming || isUploadingAttachments}
                    title={isUploadingAttachments ? "Uploading attachments…" : "Send message"}
                  >
                    {isUploadingAttachments ? "…" : "Send"}
                  </button>
                </div>
              </div>
            </form>
          </section>
        </main>
      </div>
    </div>
  );
}

// ── Copy-to-clipboard button ─────────────────────────────────────────────
//
// Shows "Copy" by default and transitions to "Copied" for 1.5s after a
// successful write. Uses the async clipboard API (available in all
// supported browsers on https/localhost).

function CopyButton({
  text,
  title = "Copy reply to clipboard",
  variant = "default",
}: {
  text: string;
  title?: string;
  variant?: "default" | "compact";
}) {
  const [state, setState] = useState<"idle" | "ok" | "err">("idle");
  const resetRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (resetRef.current) window.clearTimeout(resetRef.current);
    };
  }, []);

  const onClick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setState("ok");
    } catch {
      setState("err");
    }
    resetRef.current = window.setTimeout(() => setState("idle"), 1500);
  };

  return (
    <button
      type="button"
      onClick={() => void onClick()}
      className={
        variant === "compact"
          ? "assistant-markdown-pre-copy"
          : "touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
      }
      title={title}
    >
      {state === "idle" ? "Copy" : state === "ok" ? "Copied" : "Copy failed"}
    </button>
  );
}

// ── Context-usage ring ───────────────────────────────────────────────────
//
// Replaces the plain "compact this chat" icon button next to the
// composer. Reads as a tiny progress ring: empty when the context is
// cold, fills as the latest turn's prompt_tokens grows toward the
// model's context_length, and color-shifts through three severity
// bands (muted → accent → danger). The whole ring is the click
// target — it opens the same compact-confirm modal the old icon
// button did, so users get visual context AND a one-tap way to act on
// it from the same control.
//
// Pre-first-turn (no usage data) we fall back to the old summarize
// icon so the control still functions, just without the meter — the
// catalog might not have loaded yet, or the model's context_length
// is missing from the listing.
function ContextRing({
  usage,
  isSummarizing,
  disabled,
  onClick,
}: {
  usage: ContextUsage | null;
  isSummarizing: boolean;
  disabled: boolean;
  onClick: () => void;
}) {
  // SVG ring math. r=8 keeps the ring at roughly the same visual
  // weight as the surrounding icon buttons (paperclip, wrench) — a
  // small affordance inside the h-7 (28px) button rather than filling
  // it edge-to-edge. Two strokes on the same circle: a muted "rail"
  // full circle plus a colored progress arc whose length =
  // circumference * fraction. Stroke ends rounded so low fills don't
  // read as a sharp splinter.
  const r = 8;
  const circumference = 2 * Math.PI * r;
  const fraction = usage?.fraction ?? 0;
  const cappedFraction = Math.min(1, fraction);
  const dashFilled = circumference * cappedFraction;
  const dashEmpty = circumference - dashFilled;

  // Color follows the same severity bands as the stats-panel chip so
  // both surfaces speak with one voice.
  const arcColor =
    usage?.severity === "danger"
      ? "var(--color-danger)"
      : usage?.severity === "warn"
        ? "var(--color-accent)"
        : "var(--color-text-muted)";

  // Same defensive clamp as formatContextUsage: if fraction > 1 we
  // show "100%+" instead of an alarming impossible number. See the
  // doc in lib/contextUsage.ts for the catalog-staleness / legacy-
  // summary cases this guards against.
  const pct =
    usage?.fraction !== undefined
      ? usage.fraction > 1
        ? "100%+"
        : `${Math.max(0, Math.round(usage.fraction * 100))}%`
      : null;
  const titleText = (() => {
    if (isSummarizing) return "Compacting conversation…";
    if (!usage) return "Compact this conversation — replace earlier turns with a short summary so the next turn fits in the model's context window and stays cheap.";
    return `${formatContextUsage(usage)} — click to compact and free up context.`;
  })();

  return (
    <button
      type="button"
      aria-label={isSummarizing ? "Compacting conversation…" : pct !== null ? `Context ${pct} full — click to compact` : "Compact this conversation"}
      title={titleText}
      disabled={disabled}
      onClick={onClick}
      className="group inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-[var(--color-text-secondary)] transition hover:opacity-80 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-40"
    >
      <svg width="28" height="28" viewBox="0 0 28 28" aria-hidden="true">
        {/* Rail — full muted circle. Always rendered, even before  */}
        {/* the first turn summary lands, so the user doesn't see   */}
        {/* an icon flicker into a ring when the meter first        */}
        {/* arrives. Empty ring = "no usage data yet," not a       */}
        {/* different control.                                      */}
        <circle
          cx="14"
          cy="14"
          r={r}
          fill="none"
          stroke="var(--color-border-strong)"
          strokeWidth="2"
        />
        {/* Progress arc — rotated -90deg so the fill starts at the */}
        {/* top of the circle and walks clockwise. Length is 0 when */}
        {/* usage is null, which renders as nothing on top of the   */}
        {/* rail.                                                   */}
        {usage ? (
          <circle
            cx="14"
            cy="14"
            r={r}
            fill="none"
            stroke={arcColor}
            strokeWidth="2"
            strokeLinecap="round"
            strokeDasharray={`${dashFilled} ${dashEmpty}`}
            transform="rotate(-90 14 14)"
            style={{ transition: "stroke-dasharray 240ms ease, stroke 240ms ease" }}
          />
        ) : null}
      </svg>
    </button>
  );
}

// ── Turn summary chip ────────────────────────────────────────────────────
//
// Compact, one-line footer under the assistant reply showing what the
// turn cost, how long it took, and how many tools ran. Deliberately
// understated — this is a trust signal ("yes, you spent 3 cents"), not
// the hero content.

function TurnSummaryChip({
  summary,
  toolCalls,
  pythonStreams,
}: {
  summary: TurnSummary;
  toolCalls: ToolCall[];
  pythonStreams: PythonStream[];
}) {
  const parts: ReactNode[] = [];
  const executionCount = toolCalls.length + pythonStreams.length;
  parts.push(<span key="cost">{formatUsd(summary.costUsd)}</span>);
  if (summary.durationMs > 0) parts.push(<span key="duration">{formatDuration(summary.durationMs)}</span>);
  if (executionCount > 0) {
    parts.push(
      <span key="execution">
        {toolCalls.length > 0 ? `${toolCalls.length} tool${toolCalls.length === 1 ? "" : "s"}` : null}
        {toolCalls.length > 0 && pythonStreams.length > 0 ? " + " : null}
        {pythonStreams.length > 0 ? `${pythonStreams.length} log${pythonStreams.length === 1 ? "" : "s"}` : null}
      </span>,
    );
  }
  const tokens = summary.promptTokens + summary.completionTokens;
  if (tokens > 0) parts.push(<span key="tokens">{formatTokens(tokens)} tokens</span>);
  const pct = cachedPercent(summary);
  if (pct !== null) parts.push(<span key="cached">{pct}% cached</span>);
  const modelLabel = shortModelName(summary.model);
  if (modelLabel) parts.push(<span key="model">{modelLabel}</span>);

  return (
    <p className="text-[0.7rem] text-[var(--color-text-muted)] tabular-nums self-start break-words">
      {summary.cancelled ? "stopped · " : ""}
      {parts.map((part, index) => (
        <span key={index}>
          {index > 0 ? " · " : null}
          {part}
        </span>
      ))}
    </p>
  );
}

/**
 * SummaryBanner renders a compaction marker — the message inserted by
 * the user-initiated "summarize and continue" flow. Reads as a
 * distinct phase boundary, not a regular assistant turn:
 *
 *   - Bordered, slightly accented background so the eye lands on it.
 *   - Caption naming the model that produced the summary + token cost
 *     (folded into ConversationTotalsChip elsewhere; surfaced here
 *     only as model attribution).
 *   - Toggle to expand/collapse the pre-summary scroll.
 *   - Re-summarize button — replace semantics on the backend means a
 *     new call swaps this banner's content for a fresher summary.
 *
 * Earlier turns above the banner are dimmed (handled in the parent
 * loop) so users grok they remain in the transcript for reference but
 * are no longer in the model's live context.
 */
function SummaryBanner({
  message,
  collapsedRangeCount,
  summaryExpanded,
  onToggleExpand,
  onResummarize,
  isSummarizing,
}: {
  message: Message;
  collapsedRangeCount: number;
  summaryExpanded: boolean;
  onToggleExpand: () => void;
  onResummarize: () => void;
  isSummarizing: boolean;
}) {
  const meta = message.summaryMeta;
  return (
    <div className="rounded-[0.85rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] p-3 sm:p-4">
      <div className="flex items-center justify-between gap-2 pb-2">
        <span className="inline-flex items-center gap-1.5 text-[0.72rem] font-medium uppercase tracking-wide text-[var(--color-text-muted)]">
          <Icon name="info" className="size-3.5" aria-hidden="true" />
          Conversation summary
        </span>
        <span className="flex items-center gap-1">
          {collapsedRangeCount > 0 ? (
            <button
              type="button"
              className="rounded-full px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={onToggleExpand}
              aria-expanded={summaryExpanded}
            >
              {summaryExpanded ? `Collapse earlier ${collapsedRangeCount} turn${collapsedRangeCount === 1 ? "" : "s"}` : `Show earlier ${collapsedRangeCount} turn${collapsedRangeCount === 1 ? "" : "s"}`}
            </button>
          ) : null}
          <button
            type="button"
            className="rounded-full px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-50"
            onClick={onResummarize}
            disabled={isSummarizing}
            title="Compact again against the current scroll. Replace semantics: this banner's text is swapped, not chained."
          >
            {isSummarizing ? "Compacting…" : "Compact again"}
          </button>
        </span>
      </div>
      <div className="whitespace-pre-wrap text-[0.86rem] leading-[1.55] text-[var(--color-text-primary)] sm:text-[0.92rem]">
        {message.content}
      </div>
      {meta?.model || (meta?.costUsd ?? 0) > 0 ? (
        <p className="mt-2 text-[0.65rem] text-[var(--color-text-muted)] tabular-nums">
          {meta?.model ? <>Compacted by <span className="font-medium">{shortModelName(meta.model)}</span></> : null}
          {(meta?.costUsd ?? 0) > 0 ? <> · {meta!.costUsd! < 0.01 ? `$${meta!.costUsd!.toFixed(4)}` : `$${meta!.costUsd!.toFixed(2)}`}</> : null}
          {meta?.promptTokens ? <> · {meta.promptTokens.toLocaleString()} prompt tokens condensed</> : null}
        </p>
      ) : null}
    </div>
  );
}

/**
 * ConversationTotalsChip sits above the composer and answers the "is caching
 * actually paying off overall, and what has this chat cost me?" question in
 * one glanceable line. Hidden until at least one turn has completed.
 *
 * Rendered in the same muted micro-type as the per-turn chip so the two read
 * as a set rather than competing signals. On mobile we drop the "turns"
 * suffix if there's no room — cost + cache% are the load-bearing numbers.
 */
function ConversationTotalsChip({
  messages,
  usage,
}: {
  messages: Message[];
  // Provided by the parent so both the stats panel and the composer
  // ring read from the same computation — no duplicate memos, no
  // chance of the two surfaces disagreeing.
  usage: ContextUsage | null;
}) {
  const summaries = useMemo(
    () =>
      messages
        .map((m) => m.summary)
        .filter((s): s is TurnSummary => !!s),
    [messages],
  );

  if (summaries.length === 0) return null;
  const totals = conversationTotals(summaries);
  if (totals.turns === 0) return null;

  const parts: string[] = [];
  parts.push(`Σ ${formatUsd(totals.costUsd)}`);
  if (totals.cachedPercent !== null) parts.push(`${totals.cachedPercent}% cached`);
  parts.push(`${totals.turns} turn${totals.turns === 1 ? "" : "s"}`);
  if (usage) parts.push(formatContextUsage(usage));

  // Only the context-usage cell color-shifts; the rest stays muted so
  // the bar reads as a single chip with one variable bit.
  const contextColor =
    usage?.severity === "danger"
      ? "var(--color-danger,#dc2626)"
      : usage?.severity === "warn"
        ? "var(--color-warning,#d97706)"
        : "var(--color-text-muted)";

  return (
    <div className="flex flex-col items-end gap-0.5">
      <p
        className="text-[0.7rem] text-[var(--color-text-muted)] tabular-nums text-right"
        aria-label="Conversation totals"
        title="Cumulative cost, token-weighted cache-hit rate, and context-window usage across this chat."
      >
        {parts.slice(0, -1).join(" · ")}
        {usage ? (
          <>
            {parts.length > 1 ? " · " : ""}
            <span style={{ color: contextColor }}>{parts[parts.length - 1]}</span>
          </>
        ) : (
          parts[parts.length - 1]
        )}
      </p>
      {shouldShowDegradationCaption(usage) ? (
        <p
          className="text-[0.65rem] text-[var(--color-text-muted)] text-right italic"
          role="note"
        >
          Long chats may produce shallower analysis — context fills, recall fades.
        </p>
      ) : null}
    </div>
  );
}

function formatUsd(v: number): string {
  if (!v) return "$0.00";
  if (v < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m ${Math.floor((ms % 60_000) / 1000)}s`;
}

function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

// ── Python output block ──────────────────────────────────────────────────
//
// Renders a run_python result as terminal-style output — stdout in the
// default monospace color, stderr tinted red. Empty output is suppressed
// so we don't render an empty black box. Execution time (when the bridge
// reports it) is shown as a small footer.

function PythonOutput({ stream }: { stream: PythonStream }) {
  const stdout = stream.stdout ?? "";
  const stderr = stream.stderr ?? "";
  const error = stream.error ?? "";
  const hasErr = Boolean(stderr.trim() || error.trim());
  // Always start collapsed. Line count is a poor signal for "is this
  // small enough to inline" — a single line can be a 5000-char pandas
  // repr that bleeds across the chat column on mobile. User taps the
  // header to reveal.
  const [expanded, setExpanded] = useState(false);
  // If everything is blank, skip the block entirely. Placed AFTER
  // the useState call so React sees hooks in the same order every
  // render (rules-of-hooks).
  if (!stdout.trim() && !hasErr && !stream.executionMs) {
    return null;
  }
  const stdoutLines = stdout ? stdout.split("\n").length : 0;
  const summaryBits = [
    stdoutLines ? `${stdoutLines} line${stdoutLines === 1 ? "" : "s"}` : "",
    stream.executionMs ? `${stream.executionMs}ms` : "",
  ].filter(Boolean).join(" · ");

  return (
    <div className="min-w-0 max-w-full overflow-hidden rounded-[0.75rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)]">
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className="flex w-full items-center justify-between gap-3 px-3 py-1.5 text-[0.72rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
      >
        <span className="flex items-center gap-2">
          <span aria-hidden>{expanded ? "▾" : "▸"}</span>
          <span>python output{summaryBits ? ` · ${summaryBits}` : ""}</span>
          {hasErr ? (
            <span className="rounded-full border px-1.5 text-[0.62rem]" style={{ borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }}>
              error
            </span>
          ) : null}
        </span>
        <span className="text-[var(--color-text-muted)]">{expanded ? "collapse" : "expand"}</span>
      </button>
      {expanded ? (
        <div
          className="border-t border-[var(--color-border)] px-3 py-2 text-[0.78rem] leading-[1.55]"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {stdout ? <pre className="overflow-x-auto whitespace-pre-wrap text-[var(--color-text-primary)]">{stdout}</pre> : null}
          {stderr ? (
            <pre className="mt-1 overflow-x-auto whitespace-pre-wrap" style={{ color: "var(--color-danger)" }}>
              {stderr}
            </pre>
          ) : null}
          {error ? (
            <pre className="mt-1 overflow-x-auto whitespace-pre-wrap" style={{ color: "var(--color-danger)" }}>
              {error}
            </pre>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// ── User bubble with inline edit ─────────────────────────────────────────
//
// The most-recent user message gets an "Edit" affordance. Editing submits
// a replacement and truncates the prior turn on the server so the assistant
// regenerates from the edit. Older user turns are read-only to keep the
// transcript coherent.

// PendingAttachmentChip renders a single attachment chip in the composer's
// pending tray. Image attachments get a small inline thumbnail preview built
// from a blob URL of the local File so the user can see what's about to be
// sent — useful confirmation when an image is being attached for vision
// input. Non-image attachments fall back to the original paperclip chip.
function PendingAttachmentChip({
  attachment,
  onRemove,
  removalDisabled,
}: {
  attachment: PendingAttachment;
  onRemove: () => void;
  removalDisabled: boolean;
}) {
  const isImage = isImageMimeOrName(attachment.mime, attachment.name);
  // Compute the blob URL synchronously via useMemo so we don't trigger a
  // cascading re-render on first paint. The URL is owned by this component
  // for as long as the attachment chip is mounted; the cleanup-effect below
  // revokes it when the component unmounts or the underlying File changes
  // (which happens when the user removes the chip and adds a new one).
  const previewUrl = useMemo(
    () => (isImage ? URL.createObjectURL(attachment.file) : null),
    [isImage, attachment.file],
  );
  useEffect(() => {
    if (!previewUrl) return;
    return () => URL.revokeObjectURL(previewUrl);
  }, [previewUrl]);

  return (
    <span
      className={[
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-1 text-[0.72rem]",
        attachment.status === "error"
          ? "border-[var(--color-danger,#dc2626)] text-[var(--color-danger,#dc2626)]"
          : "border-[var(--color-border-strong)] text-[var(--color-text-secondary)]",
        attachment.status === "uploading" ? "opacity-60" : "",
      ].join(" ")}
      title={`${attachment.name} (${formatBytes(attachment.size)})`}
    >
      {isImage && previewUrl ? (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={previewUrl}
          alt=""
          className="size-4 rounded-sm object-cover"
          draggable={false}
        />
      ) : (
        <Icon name="paperclip" className="size-3" />
      )}
      <span className="max-w-[14rem] truncate">{attachment.name}</span>
      <span className="text-[0.65rem] text-[var(--color-text-muted)]">
        {formatBytes(attachment.size)}
      </span>
      <button
        type="button"
        aria-label={`Remove ${attachment.name}`}
        className="touch-target-hit text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
        disabled={removalDisabled}
        onClick={onRemove}
      >
        <Icon name="close" className="size-3" />
      </button>
    </span>
  );
}

// isImageMimeOrName returns true when an attachment looks like an image based
// on its declared MIME (preferred) or filename extension (fallback for clients
// that omit the Content-Type — e.g. some drag-and-drop sources). Mirrors the
// allow list the Go side uses in tools.IsImageMIME / tools.ImageMIMEFromName.
function isImageMimeOrName(mime: string, name: string): boolean {
  const m = mime.trim().toLowerCase();
  if (m === "image/png" || m === "image/jpeg" || m === "image/jpg" || m === "image/gif" || m === "image/webp") {
    return true;
  }
  const idx = name.lastIndexOf(".");
  if (idx < 0) return false;
  const ext = name.slice(idx).toLowerCase();
  return ext === ".png" || ext === ".jpg" || ext === ".jpeg" || ext === ".gif" || ext === ".webp";
}

function UserBubble({
  message,
  isLastUser,
  isStreaming,
  onResend,
}: {
  message: Message;
  isLastUser: boolean;
  isStreaming: boolean;
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
        {renderAssistantContent(message.content) ?? message.content}
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
    <div className="rounded-[0.95rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_68%,transparent)] px-3 py-2 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]">
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
      {expanded ? <div className="mt-2">{renderAssistantContent(text)}</div> : null}
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
    <div className="rounded-[0.95rem] border border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_72%,transparent)] px-4 py-3 text-[var(--color-text-primary)]">
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
          {renderAssistantContent(streamingText)}
        </div>
      ) : null}
    </div>
  );
}

// ── Approval card (send_email human-in-the-loop) ─────────────────────────

// Preview viewport presets for the inline email preview. These mirror the
// widths real clients render at — 375px ≈ iPhone portrait, 700px is the
// canonical desktop email width (matches the design-system viewer we
// lifted this pattern from). Anything wider than ~800 is unrealistic for
// real email clients, so we don't expose it.
type PreviewViewport = "mobile" | "desktop";
const PREVIEW_WIDTHS: Record<PreviewViewport, number> = {
  mobile: 375,
  desktop: 700,
};

// "inbox" swaps the stage background (not the email body) to mimic how a
// given email reads against a light vs dark client chrome. We deliberately
// do NOT try to simulate Outlook/Gmail's forced color inversion — that
// needs same-origin iframe scripting, which conflicts with the sandbox
// attribute we rely on for safety.
type PreviewInbox = "light" | "dark";

function ApprovalCard({
  approval,
  conversationId,
  onResolved,
  onModelSwitched,
  onSwitchAndRetry,
}: {
  approval: Approval;
  conversationId: string;
  onResolved: (next: Approval) => void;
  // suggest_advanced_model only: callback fired after the server confirms
  // the conversation has been pinned to a new model. Lets the caller sync
  // its local selectedModel state without a refetch.
  onModelSwitched?: (model: string) => void;
  // suggest_advanced_model only: fired AFTER onModelSwitched when the user
  // picked "Switch & retry". The caller is expected to re-submit the
  // prior user turn under the newly-pinned model.
  onSwitchAndRetry?: () => void | Promise<void>;
}) {
  const [submitting, setSubmitting] = useState<"send" | "cancel" | null>(null);
  // Both card kinds auto-expand: preview because seeing the render IS
  // the feature, send because users were missing the Send button when
  // the card landed below an already-expanded preview iframe and the
  // collapsed send card looked like just another header. Visual
  // differentiation (border style + tint) now does the disambiguation.
  const [expanded, setExpanded] = useState(true);
  const [viewport, setViewport] = useState<PreviewViewport>("desktop");
  const [inbox, setInbox] = useState<PreviewInbox>("light");
  const [showRaw, setShowRaw] = useState(false);

  const resolve = async (approved: boolean) => {
    if (submitting || approval.status !== "pending" || !conversationId) return;
    setSubmitting(approved ? "send" : "cancel");
    try {
      const response = await fetch(
        `/api/conversations/${conversationId}/approvals/${approval.id}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ approved }),
        },
      );
      if (!response.ok) {
        onResolved({ ...approval, status: "failed", resultText: await response.text() });
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        result_text?: string;
        is_err?: boolean;
      };
      onResolved({
        ...approval,
        status: data.is_err ? "failed" : data.status,
        resultText: data.result_text,
      });
    } catch (err) {
      onResolved({
        ...approval,
        status: "failed",
        resultText: err instanceof Error ? err.message : "Request failed.",
      });
    } finally {
      setSubmitting(null);
    }
  };

  if (approval.tool === "bash") {
    return (
      <BashApprovalCard
        approval={approval}
        submitting={submitting}
        onResolve={resolve}
      />
    );
  }

  if (approval.tool === "suggest_advanced_model") {
    return (
      <SuggestAdvancedModelCard
        approval={approval}
        conversationId={conversationId}
        onResolved={onResolved}
        onModelSwitched={onModelSwitched}
        onSwitchAndRetry={onSwitchAndRetry}
      />
    );
  }

  const recipients = toRecipientList(approval.summary.to, approval.summary.cc, approval.summary.bcc);
  const subject = approval.summary.subject ?? "(no subject)";
  const from = approval.summary.from ?? "";
  const preview = approval.summary.preview ?? "";
  // Full body comes through on the new summary.content field; older
  // sessions that pre-date it will fall back to the truncated preview.
  const fullContent = approval.summary.content ?? preview;
  const isHtml = (approval.summary.content_type ?? "").toLowerCase().startsWith("text/html");
  const contentOverflow = approval.summary.content_overflow ?? false;
  const hasBody = fullContent.length > 0;

  // preview_email uses the same card chrome but means "look, don't
  // send." Auto-expand ON FIRST RENDER (the whole point of the card)
  // but still let the user collapse. expanded defaults to true
  // via the initializer below.
  const isPreviewOnly = approval.tool === "preview_email";
  const effectiveExpanded = expanded;

  // Pending preview and pending send used to share the same accent border,
  // which made stacked cards visually indistinguishable. They now diverge:
  // preview reads as a quiet draft frame (muted border, dashed via the
  // className below, no background tint); send reads as an action card
  // (accent border + faint accent-tinted background).
  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? isPreviewOnly
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : isPreviewOnly
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
            : {
                borderColor: "var(--color-accent)",
                color: "var(--color-text-primary)",
                background:
                  "color-mix(in srgb, var(--color-accent) 14%, var(--color-overlay-soft))",
              };

  // Lead the title with an uppercase intent tag so a skimmer's eye
  // distinguishes draft cards from action cards before reading the rest.
  // The pending verb-phrase is what every existing e2e test asserts on
  // (e.g. /Send this email\?/i) — substring matches still pass.
  const title = isPreviewOnly
    ? approval.status === "pending"
      ? "DRAFT · Email preview (not sent)"
      : approval.status === "approved" || approval.status === "rejected"
        ? "Draft dismissed"
        : "Preview failed"
    : approval.status === "pending"
      ? "ACTION REQUIRED · Send this email?"
      : approval.status === "approved"
        ? "Email sent ✓"
        : approval.status === "rejected"
          ? "Send cancelled"
          : "Send failed";

  // Pending preview gets a dashed border to signal "draft / not real /
  // sketch." Pending send keeps the solid border (the default `border`
  // class) so the two cards are distinguishable at a glance even before
  // a user reads the title. Resolved states fall back to solid for both
  // — once a card is finalized the draft/action distinction no longer
  // matters and the success/danger color tells the story.
  const isPendingPreview = isPreviewOnly && approval.status === "pending";
  const borderStyleClass = isPendingPreview ? " border-dashed" : "";
  // No tint override needed for pending send — the inline `background`
  // in statusStyle replaces the default. For all other states keep the
  // existing soft overlay.
  const bgClass = approval.status === "pending" && !isPreviewOnly
    ? ""
    : " bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)]";

  return (
    <div
      data-approval-id={approval.id}
      className={`rounded-[0.95rem] border px-3 py-2.5 text-[0.8125rem] leading-[1.5]${borderStyleClass}${bgClass}`}
      style={statusStyle}
    >
      <div className="mb-2 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <span aria-hidden>{isPreviewOnly ? "👁" : "📤"}</span>
          <span className="font-medium">{title}</span>
        </div>
        {hasBody ? (
          <button
            type="button"
            className="text-[0.72rem] underline text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            onClick={() => setExpanded((v) => !v)}
          >
            {/*
             * Send cards never use the word "preview" in the toggle —
             * a user hunting for the Send button used to mistake the
             * collapsed send card's "Preview email" toggle for the
             * preview card itself. Preview cards keep the original
             * "preview" wording since that's literally what they are.
             */}
            {isPreviewOnly
              ? expanded ? "Hide preview" : "Show preview"
              : expanded ? "Hide email" : "Show email"}
          </button>
        ) : null}
      </div>

      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {recipients ? <div><span className="text-[var(--color-text-muted)]">To: </span>{recipients}</div> : null}
        <div><span className="text-[var(--color-text-muted)]">Subject: </span>{subject}</div>
        {from ? <div><span className="text-[var(--color-text-muted)]">From: </span>{from}</div> : null}
      </div>

      {effectiveExpanded && hasBody ? (
        <EmailPreview
          html={fullContent}
          isHtml={isHtml}
          viewport={viewport}
          inbox={inbox}
          showRaw={showRaw}
          contentOverflow={contentOverflow}
          onViewportChange={setViewport}
          onInboxChange={setInbox}
          onShowRawChange={setShowRaw}
        />
      ) : null}

      {approval.status === "pending" ? (
        <div className="mt-3 flex items-center gap-2">
          {isPreviewOnly ? (
            <button
              type="button"
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => void resolve(true)}
            >
              {submitting === "send" ? "Dismissing…" : "Dismiss"}
            </button>
          ) : (
            <>
              <button
                type="button"
                className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
                disabled={submitting !== null}
                onClick={() => void resolve(true)}
              >
                {submitting === "send" ? "Sending…" : "Send"}
              </button>
              <button
                type="button"
                className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
                disabled={submitting !== null}
                onClick={() => void resolve(false)}
              >
                {submitting === "cancel" ? "Cancelling…" : "Cancel"}
              </button>
            </>
          )}
        </div>
      ) : approval.resultText ? (
        <p className="mt-2 text-[0.72rem] text-[var(--color-text-muted)]">{approval.resultText}</p>
      ) : null}
    </div>
  );
}

function BashApprovalCard({
  approval,
  submitting,
  onResolve,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  onResolve: (approved: boolean) => void;
}) {
  const command = approval.summary.command ?? approval.summary.preview ?? "";
  const workingDir = approval.summary.working_dir ?? "";
  const timeoutSec = approval.summary.timeout_seconds ?? 0;

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  return (
    <div
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>⚡</span>
        <span className="font-medium">
          {approval.status === "pending"
            ? "Run this shell command?"
            : approval.status === "approved"
              ? "Command executed"
              : approval.status === "rejected"
                ? "Command declined"
                : "Command failed"}
        </span>
      </div>

      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 font-mono text-[0.78rem] text-[var(--color-text-primary)]">
        {command}
      </pre>

      {(workingDir || timeoutSec) ? (
        <div className="mt-1 grid gap-0.5 text-[0.72rem] text-[var(--color-text-muted)]">
          {workingDir ? <div>cwd: {workingDir}</div> : null}
          {timeoutSec ? <div>timeout: {timeoutSec}s</div> : null}
        </div>
      ) : null}

      {approval.status === "pending" ? (
        <div className="mt-3 flex items-center gap-2">
          <button
            type="button"
            className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
            disabled={submitting !== null}
            onClick={() => onResolve(true)}
          >
            {submitting === "send" ? "Running…" : "Approve & run"}
          </button>
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={submitting !== null}
            onClick={() => onResolve(false)}
          >
            {submitting === "cancel" ? "Cancelling…" : "Cancel"}
          </button>
        </div>
      ) : approval.resultText ? (
        <pre className="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 font-mono text-[0.72rem] text-[var(--color-text-muted)]">
          {approval.resultText}
        </pre>
      ) : null}
    </div>
  );
}

// SuggestAdvancedModelCard renders an inline nudge when the agent
// detects it's stuck on a workload that the advanced tier would handle
// better. Three actions:
//   - Switch & retry (default): pin the conversation to advanced and
//     immediately re-submit the prior user turn under the new model.
//   - Just switch: pin to advanced; the user will compose the next
//     prompt themselves.
//   - Dismiss: close the card. The server records a rejection so the
//     gate's user-turn cooldown applies before another suggestion.
//
// The server is authoritative on the recommended slug
// (agent.AdvancedModelSlug) — we don't accept it from the agent.
type SuggestAction = "switch_and_retry" | "switch_only" | "dismiss";

function SuggestAdvancedModelCard({
  approval,
  conversationId,
  onResolved,
  onModelSwitched,
  onSwitchAndRetry,
}: {
  approval: Approval;
  conversationId: string;
  onResolved: (next: Approval) => void;
  onModelSwitched?: (model: string) => void;
  onSwitchAndRetry?: () => void | Promise<void>;
}) {
  const [pending, setPending] = useState<SuggestAction | null>(null);

  const reason = approval.summary.reason ?? "Advanced mode would handle this better.";
  const recommendedSlug = approval.summary.recommend_model ?? ADVANCED_MODEL;
  const recommendedLabel = labelForModel(recommendedSlug);

  const submit = async (action: SuggestAction) => {
    if (pending || approval.status !== "pending" || !conversationId) return;
    setPending(action);
    try {
      const response = await fetch(
        `/api/conversations/${conversationId}/approvals/${approval.id}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            approved: action !== "dismiss",
            action,
          }),
        },
      );
      if (!response.ok) {
        onResolved({ ...approval, status: "failed", resultText: await response.text() });
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        action?: SuggestAction;
        model?: string;
        result_text?: string;
      };
      onResolved({
        ...approval,
        status: data.status,
        resultText: data.result_text,
      });
      if (data.status === "approved" && data.model) {
        onModelSwitched?.(data.model);
        if (action === "switch_and_retry") {
          await onSwitchAndRetry?.();
        }
      }
    } catch (err) {
      onResolved({
        ...approval,
        status: "failed",
        resultText: err instanceof Error ? err.message : "Request failed.",
      });
    } finally {
      setPending(null);
    }
  };

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const title =
    approval.status === "pending"
      ? `Try ${recommendedLabel} for the rest of this chat?`
      : approval.status === "approved"
        ? `Switched to ${recommendedLabel}`
        : approval.status === "rejected"
          ? "Suggestion dismissed"
          : "Suggestion failed";

  return (
    <div
      data-approval-id={approval.id}
      data-tool="suggest_advanced_model"
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>✨</span>
        <span className="font-medium">{title}</span>
      </div>
      <p className="text-[0.78rem] text-[var(--color-text-secondary)]">{reason}</p>

      {approval.status === "pending" ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <button
            type="button"
            className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_and_retry")}
          >
            {pending === "switch_and_retry" ? "Switching…" : "Switch & retry"}
          </button>
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_only")}
          >
            {pending === "switch_only" ? "Switching…" : "Just switch"}
          </button>
          <button
            type="button"
            className="rounded-full border border-transparent px-3 py-1.5 text-[0.75rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("dismiss")}
          >
            {pending === "dismiss" ? "Dismissing…" : "Dismiss"}
          </button>
        </div>
      ) : approval.resultText ? (
        <p className="mt-2 text-[0.72rem] text-[var(--color-text-muted)]">{approval.resultText}</p>
      ) : null}
    </div>
  );
}

// WorkspaceImage renders an <img> from the chat workspace with the
// settings that keep it from flickering when the user scrolls.
//
// Three fixes layered together:
//   - React.memo: parent re-renders triggered by scroll (the
//     showJumpToLatest visibility update fires on every scroll tick,
//     so without memoization every tick reconciles a fresh <img>
//     tree and mobile browsers blank the paint for a frame).
//   - loading="eager": once the agent shows the user a chart it's
//     intentional content, not a long-article tail. Lazy loading
//     plus aggressive mobile-browser memory unloads on scroll-away
//     was the biggest source of flicker — re-entering viewport
//     would re-fetch and re-decode.
//   - decoding="async": lets the browser decode off the main thread
//     so the scroll keeps its frame budget while the image paints.
const WorkspaceImage = memo(function WorkspaceImage({
  src,
  alt,
  title,
}: {
  src: string;
  alt: string;
  title?: string;
}) {
  const [errored, setErrored] = useState(false);
  if (errored) {
    return (
      <span className="my-2 inline-block rounded-md border border-dashed border-[var(--color-border-strong)] px-2 py-1 text-[0.72rem] text-[var(--color-text-muted)]">
        couldn&rsquo;t load image: {alt || src}
      </span>
    );
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt={alt}
      title={title}
      loading="eager"
      decoding="async"
      className="my-2 block max-w-full rounded-[0.5rem] border border-[var(--color-border)]"
      onError={() => setErrored(true)}
    />
  );
});

// InlineHtmlPreview renders a ```html code block from an assistant
// message as a sandboxed iframe. Uses sandbox="" (most restrictive —
// no scripts, no forms, no top-navigation) so arbitrary LLM-generated
// HTML is inert. The "Show source" toggle lets the user flip back to
// the raw code when they want to copy or inspect it.
//
// While the assistant message is still streaming, we deliberately do
// NOT mount the iframe AND don't render the partial source either —
// every new streaming chunk would otherwise either rebuild the iframe
// DOM against malformed HTML (jank-flickers on desktop) or push a
// growing one-line text blob through the parent flex layout (jank-
// flickers on mobile, base64 image data became a single 17K-char line
// that thrashed reflow). A static "Building preview…" placeholder lets
// the rest of the streaming text flow normally; the iframe mounts once
// the turn completes.
function InlineHtmlPreview({ html, isStreaming = false, conversationId }: { html: string; isStreaming?: boolean; conversationId?: string | null }) {
  // Inject a <base> tag so relative image/link paths in the LLM-generated HTML
  // resolve to the workspace API. This allows charts and other files generated
  // by the agent to render correctly inside the sandboxed iframe.
  let processedHtml = html;
  if (conversationId && conversationId !== PENDING_CONV_KEY) {
    const baseHref = `/api/conversations/${encodeURIComponent(conversationId)}/workspace/`;
    const baseTag = `<base href="${baseHref}">`;
    if (/<head[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(/(<head[^>]*>)/i, `$1\n${baseTag}`);
    } else if (/<html[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(/(<html[^>]*>)/i, `$1\n<head>\n${baseTag}\n</head>`);
    } else if (/<!DOCTYPE[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(/(<!DOCTYPE[^>]*>)/i, `$1\n<head>\n${baseTag}\n</head>`);
    } else {
      processedHtml = `<head>\n${baseTag}\n</head>\n${processedHtml}`;
    }
  }
  const [showSource, setShowSource] = useState(false);
  if (isStreaming) {
    return (
      <div className="my-2 flex items-center gap-2 rounded-[0.6rem] border border-dashed border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.72rem] text-[var(--color-text-muted)]">
        <span className="thinking-dots" aria-hidden="true">
          <span className="thinking-dot" />
          <span className="thinking-dot" />
          <span className="thinking-dot" />
        </span>
        <span>Building HTML preview ({html.length.toLocaleString()} chars so far)…</span>
      </div>
    );
  }
  return (
    <div className="my-2 overflow-hidden rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)]">
      <div className="flex items-center justify-between border-b border-[var(--color-border)] px-2 py-1 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
        <span>HTML preview</span>
        <button
          type="button"
          onClick={() => setShowSource((v) => !v)}
          className="rounded-full border border-[var(--color-border)] px-2 py-0.5 text-[0.62rem] normal-case tracking-normal text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
        >
          {showSource ? "Show preview" : "Show source"}
        </button>
      </div>
      {showSource ? (
        <pre
          className="overflow-auto p-2 text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
          style={{ fontFamily: "var(--font-code)", maxHeight: "24rem" }}
        >
          {html}
        </pre>
      ) : (
        <iframe
          srcDoc={processedHtml}
          sandbox=""
          title="HTML preview"
          className="w-full bg-white"
          style={{ minHeight: "20rem", height: "60vh", border: "none" }}
        />
      )}
    </div>
  );
}

// EmailPreview renders a staged email exactly as SendGrid will ship it,
// inside a sandboxed iframe so no third-party markup can touch the host
// page. The mobile/desktop toggle is a fixed-width container (not CSS
// zoom) so CSS media queries in the email body fire the same way real
// mail clients evaluate them. The inbox light/dark swap just changes
// the stage background around the iframe — we intentionally don't try
// to fake client-side color inversion (which requires same-origin
// scripting and conflicts with sandboxing).
function EmailPreview({
  html,
  isHtml,
  viewport,
  inbox,
  showRaw,
  contentOverflow,
  onViewportChange,
  onInboxChange,
  onShowRawChange,
}: {
  html: string;
  isHtml: boolean;
  viewport: PreviewViewport;
  inbox: PreviewInbox;
  showRaw: boolean;
  contentOverflow: boolean;
  onViewportChange: (v: PreviewViewport) => void;
  onInboxChange: (i: PreviewInbox) => void;
  onShowRawChange: (v: boolean) => void;
}) {
  const viewportPx = PREVIEW_WIDTHS[viewport];
  const stageBg = inbox === "dark" ? "#121212" : "#f4f6fb";
  const iframeRef = useRef<HTMLIFrameElement | null>(null);
  // Track the viewport width so the Desktop button can be hidden on
  // phones — there's no point showing a 600px inbox width on a 360px
  // screen, it'll just look like a shrunken blob.
  const [windowWidth, setWindowWidth] = useState(
    typeof window === "undefined" ? 1024 : window.innerWidth,
  );
  useEffect(() => {
    const handler = () => setWindowWidth(window.innerWidth);
    window.addEventListener("resize", handler);
    return () => window.removeEventListener("resize", handler);
  }, []);
  // Reserve a bit of room for padding around the preview stage.
  const canShowDesktop = windowWidth >= PREVIEW_WIDTHS.desktop + 40;
  // If the user picked Desktop on a wide window and then resized the
  // window narrow, fall back to Mobile so nothing looks broken.
  useEffect(() => {
    if (!canShowDesktop && viewport === "desktop") {
      onViewportChange("mobile");
    }
  }, [canShowDesktop, viewport, onViewportChange]);

  // If the payload was clipped server-side, warn the user. Rendering a
  // partial HTML document can cut off mid-tag and look broken; still
  // better than hiding the preview entirely.
  const overflowNotice = contentOverflow
    ? "Body exceeded the 1 MiB preview cap — the tail is truncated. The full email will still be sent."
    : null;

  // Dark-mode color inversion. Real email clients (Outlook dark,
  // Gmail dark, Apple Mail dark) forcibly swap light backgrounds for
  // dark ones and bump text contrast. The preview can replicate that
  // by poking at the iframe's DOM post-load — which requires
  // sandbox="allow-same-origin" (no scripts still, so LLM HTML stays
  // inert). Color maps mirror the flag repo's email-preview.html so
  // emails rendered in both places look identical.
  // Stabilized with useCallback so the effect below can list it as a real
  // dependency (honest deps, no suppression) and the iframe onLoad handler
  // gets a stable identity. It only reads `inbox` plus the stable iframe ref
  // and module-level color maps, so `[inbox]` is the complete dep set.
  const applyInboxMode = useCallback(() => {
    const frame = iframeRef.current;
    if (!frame) return;
    try {
      const doc = frame.contentDocument;
      if (!doc) return;
      // Always start from original before applying a swap so
      // toggling back and forth is clean.
      const els = doc.querySelectorAll<HTMLElement>("[data-ep-orig]");
      els.forEach((el) => {
        const orig = el.getAttribute("data-ep-orig") || "";
        el.setAttribute("style", orig);
      });
      if (inbox !== "dark") {
        return;
      }
      doc.querySelectorAll<HTMLElement>("body, body *").forEach((el) => {
        if (!el.getAttribute("data-ep-orig")) {
          el.setAttribute("data-ep-orig", el.getAttribute("style") || "");
        }
        const cs = doc.defaultView?.getComputedStyle(el);
        if (!cs) return;
        const swapBG = DARK_BG_MAP[normRgb(cs.backgroundColor)];
        if (swapBG) el.style.setProperty("background-color", swapBG, "important");
        const swapText = DARK_TEXT_MAP[normRgb(cs.color)];
        if (swapText) el.style.setProperty("color", swapText, "important");
        const swapBorder = DARK_BORDER_MAP[normRgb(cs.borderTopColor)];
        if (swapBorder) {
          el.style.setProperty("border-top-color", swapBorder, "important");
          el.style.setProperty("border-right-color", swapBorder, "important");
          el.style.setProperty("border-bottom-color", swapBorder, "important");
          el.style.setProperty("border-left-color", swapBorder, "important");
        }
      });
    } catch {
      // Cross-origin or transient load state — ignore, next onLoad retries.
    }
  }, [inbox]);
  // Re-apply when the inbox mode flips or the HTML payload changes (the
  // iframe reloads, so the previous swap is gone). `html` stays a dep
  // because applyInboxMode itself doesn't read it — it's the reload trigger.
  useEffect(() => {
    applyInboxMode();
  }, [applyInboxMode, html]);

  if (!isHtml) {
    // Plain-text emails: stick with the simple monospaced view and
    // skip the viewport toolbar. Still wrap in a bordered box so it
    // visually matches the approval card styling.
    return (
      <div className="mt-2 min-w-0 max-w-full">
        {overflowNotice ? (
          <p className="mb-1 text-[0.7rem] text-[var(--color-text-muted)]">{overflowNotice}</p>
        ) : null}
        <pre
          className="max-h-[16rem] min-w-0 max-w-full overflow-auto rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.75rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {html}
        </pre>
      </div>
    );
  }

  return (
    <div className="mt-2 min-w-0 max-w-full overflow-hidden rounded-[0.6rem] border border-[var(--color-border)]">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.7rem]">
        <div className="flex flex-wrap items-center gap-2">
          <SegGroup label="Inbox">
            <SegButton active={inbox === "light"} onClick={() => onInboxChange("light")}>Light</SegButton>
            <SegButton active={inbox === "dark"} onClick={() => onInboxChange("dark")}>Dark</SegButton>
          </SegGroup>
          {canShowDesktop ? (
            <SegGroup label="Size">
              <SegButton active={viewport === "mobile"} onClick={() => onViewportChange("mobile")}>Mobile</SegButton>
              <SegButton active={viewport === "desktop"} onClick={() => onViewportChange("desktop")}>Desktop</SegButton>
            </SegGroup>
          ) : null}
        </div>
        <button
          type="button"
          className="underline text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
          onClick={() => onShowRawChange(!showRaw)}
        >
          {showRaw ? "Hide source" : "Show source"}
        </button>
      </div>

      {overflowNotice ? (
        <p className="border-b border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.7rem] text-[var(--color-text-muted)]">
          {overflowNotice}
        </p>
      ) : null}

      <div
        className="flex justify-center px-2 py-3 transition-colors"
        style={{ background: stageBg }}
      >
        <iframe
          ref={iframeRef}
          onLoad={applyInboxMode}
          key={`${viewport}`}
          title="Email preview"
          /* allow-same-origin lets the parent touch the iframe DOM to
             apply dark-mode color swaps. No allow-scripts → arbitrary
             LLM HTML still can't execute JS. */
          sandbox="allow-same-origin"
          srcDoc={html}
          className="rounded-[0.4rem] border border-black/10 bg-white transition-[width] duration-200"
          style={{ width: `${viewportPx}px`, maxWidth: "100%", height: "28rem" }}
        />
      </div>

      {showRaw ? (
        <pre
          className="max-h-[14rem] min-w-0 max-w-full overflow-auto border-t border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {html}
        </pre>
      ) : null}
    </div>
  );
}

// Dark-mode color maps ported from flag's email-preview.html. Keys are
// normalized rgb() strings — lowercased, whitespace-stripped — because
// the browser's getComputedStyle always returns colors in that form.
// Values are the tuned dark-mode hex substitutes; contrast ratios were
// picked in flag to hit WCAG AA against the new backgrounds.
const DARK_BG_MAP: Record<string, string> = {
  "rgb(244,246,251)": "#1e1e2e",
  "rgb(250,250,250)": "#2a2a3c",
  "rgb(238,243,255)": "#252538",
  "rgb(255,247,232)": "#2e2518",
  "rgb(239,250,243)": "#1a2e20",
  "rgb(255,255,255)": "#2a2a3c",
  "rgb(98,98,160)": "#5A5494",
};
const DARK_TEXT_MAP: Record<string, string> = {
  "rgb(20,24,36)": "#e0e0e0",
  "rgb(51,65,95)": "#c0c8d8",
  "rgb(88,111,124)": "#9aa8b4",
  "rgb(92,106,135)": "#9aa8b4",
  "rgb(143,90,18)": "#f0a040",
  "rgb(28,90,51)": "#4ADE80",
  "rgb(124,31,31)": "#FF6B6B",
};
const DARK_BORDER_MAP: Record<string, string> = {
  "rgb(215,222,238)": "#3a3a50",
  "rgb(230,235,245)": "#3a3a50",
  "rgb(240,215,175)": "#5a4520",
  "rgb(205,237,216)": "#2a5035",
};

function normRgb(c: string): string {
  return c.toLowerCase().replace(/\s+/g, "");
}

function SegGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="inline-flex items-center gap-1.5">
      <span className="text-[0.68rem] uppercase tracking-wide text-[var(--color-text-muted)]">{label}</span>
      <div className="inline-flex items-center gap-0.5 rounded-full border border-[var(--color-border-strong)] p-0.5">
        {children}
      </div>
    </div>
  );
}

function SegButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      className={[
        "rounded-full px-2 py-0.5 text-[0.68rem] transition",
        active
          ? "bg-[var(--color-text-primary)] text-[var(--color-surface-1)]"
          : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
      ].join(" ")}
      onClick={onClick}
    >
      {children}
    </button>
  );
}

function toRecipientList(...groups: Array<string | string[] | undefined>): string {
  const all: string[] = [];
  for (const g of groups) {
    if (!g) continue;
    if (Array.isArray(g)) all.push(...g.filter(Boolean));
    else all.push(g);
  }
  return all.join(", ");
}

// ── Tool chip ────────────────────────────────────────────────────────────

function ToolChip({ tc, taskTrackerDisplay }: { tc: ToolCall; taskTrackerDisplay: TaskTrackerDisplay | null }) {
  const [open, setOpen] = useState(false);
  const label = prettyToolName(tc.name);
  const stateStyle: React.CSSProperties =
    tc.state === "error"
      ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
      : tc.state === "pending"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" };

  return (
    <div className="w-full min-w-0 max-w-full">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1.5 rounded-full border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-2.5 py-1 text-[0.72rem] transition hover:bg-[var(--color-overlay-soft)]"
        style={stateStyle}
        title={tc.name}
      >
        <span>{toolIcon(tc.name)}</span>
        <span className="font-medium">{label}</span>
        {tc.state === "pending" ? (
          <span className="thinking-dots" aria-hidden="true">
            <span className="thinking-dot" />
            <span className="thinking-dot" />
            <span className="thinking-dot" />
          </span>
        ) : null}
      </button>
      {open ? (
        /* min-w-0 + max-w-full keeps wide child content (long-line
           pre, code blocks, JSON with a huge inline string) from
           blowing out the chat column's width. Without this, a <pre>
           with overflow-auto will still expand its flex/grid parent
           in Chrome. */
        <div className="mt-1 grid gap-1.5 min-w-0 max-w-full">
          {/* task_tracker echoes its input in the result (the result is
              the authoritative state plus a summary), so showing both
              renders the list twice. Suppress the input view when we
              already have the result. */}
          {tc.name === "task_tracker" && tc.resultText ? null : (
            <ToolInputView name={tc.name} input={tc.input} taskTrackerDisplay={taskTrackerDisplay} />
          )}
          {tc.resultText ? (
            <ToolResultView
              name={tc.name}
              resultText={tc.resultText}
              isErr={tc.state === "error"}
              taskTrackerDisplay={taskTrackerDisplay}
            />
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// ── Per-tool input renderers ─────────────────────────────────────────────
//
// Pulls the meaningful field(s) out of the JSON tool input and renders
// them in a human-friendly form rather than dumping raw JSON. Unknown
// tools fall back to a pretty-printed JSON block (safePretty), same as
// before.

function parseJSON(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

function JsonFallback({ raw }: { raw: string }) {
  return (
    <pre
      className="overflow-x-auto rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] leading-[1.4] text-[var(--color-text-secondary)]"
      style={{ fontFamily: "var(--font-code)" }}
    >
      {safePretty(raw)}
    </pre>
  );
}

type DisplayTask = {
  title: string;
  status: "todo" | "in_progress" | "done";
  notes?: string;
};

type TaskTrackerDisplay = {
  tasks: DisplayTask[];
  summary: {
    total: number;
    todo: number;
    in_progress: number;
    done: number;
  };
  activeTask: string;
};

function parseTaskList(raw: string): DisplayTask[] {
  const parsed = parseJSON(raw);
  if (!parsed || typeof parsed !== "object") return [];
  const obj = parsed as Record<string, unknown>;
  const resultTasks = Array.isArray(obj.tasks) ? (obj.tasks as unknown[]) : [];
  const inputTasks = Array.isArray(obj.task_list) ? (obj.task_list as unknown[]) : [];
  const source = resultTasks.length > 0 ? resultTasks : inputTasks;
  return source.flatMap((entry) => {
    const task = (entry ?? {}) as Record<string, unknown>;
    if (typeof task.title !== "string" || !task.title.trim()) return [];
    const status = task.status;
    if (status !== "todo" && status !== "in_progress" && status !== "done") return [];
    return [{
      title: task.title.trim(),
      status,
      notes: typeof task.notes === "string" && task.notes.trim() ? task.notes.trim() : undefined,
    } satisfies DisplayTask];
  });
}

function summarizeDisplayTasks(tasks: DisplayTask[]) {
  const summary = { total: tasks.length, todo: 0, in_progress: 0, done: 0 };
  for (const task of tasks) {
    if (task.status === "done") summary.done += 1;
    else if (task.status === "in_progress") summary.in_progress += 1;
    else summary.todo += 1;
  }
  return summary;
}

function taskTrackerDisplayForMessage(message: Message): TaskTrackerDisplay | null {
  let tracker: ToolCall | null = null;
  const tc = message.toolCalls ?? [];
  for (let i = tc.length - 1; i >= 0; i--) {
    if (tc[i].name === "task_tracker") {
      tracker = tc[i];
      break;
    }
  }
  if (!tracker) return null;
  const baseTasks = tracker.resultText ? parseTaskList(tracker.resultText) : parseTaskList(tracker.input);
  if (baseTasks.length === 0) return null;

  const activeTask =
    baseTasks.find((task) => task.status === "in_progress")?.title ??
    baseTasks.find((task) => task.status !== "done")?.title ??
    "";

  return { tasks: baseTasks, summary: summarizeDisplayTasks(baseTasks), activeTask };
}

function CodeBlock({
  code,
  language,
  maxHeight = "16rem",
}: {
  code: string;
  language?: string;
  maxHeight?: string;
}) {
  return (
    <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
      {language ? (
        <div className="border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
          {language}
        </div>
      ) : null}
      <div className="overflow-auto px-2 py-1.5" style={{ maxHeight }}>
        {language && syntaxSupportedLanguages.has(language) ? (
          <SyntaxHighlighter
            language={language}
            style={syntaxStyle}
            PreTag="pre"
            CodeTag="code"
            wrapLongLines={false}
            customStyle={{
              background: "transparent",
              padding: 0,
              margin: 0,
              fontSize: "0.72rem",
              lineHeight: 1.4,
              fontFamily: "var(--font-code)",
            }}
            codeTagProps={{ style: { fontFamily: "var(--font-code)" } }}
          >
            {code}
          </SyntaxHighlighter>
        ) : (
          <pre
            className="text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
            style={{ fontFamily: "var(--font-code)" }}
          >
            {code}
          </pre>
        )}
      </div>
    </div>
  );
}

// Syntax highlighter: Prism via react-syntax-highlighter, light build so
// we only ship the languages we use. The style object below references
// CSS variables so it tracks the light/dark theme automatically — no
// JS theme detection or re-render needed.
const syntaxSupportedLanguages = new Set(["python", "bash", "shell", "json", "yaml"]);

// syntaxStyle is a react-syntax-highlighter style object: keys are
// Prism token classes, values are CSS-in-JS objects. We use CSS var
// references so the colors flip with the app's light/dark theme.
const syntaxStyle: Record<string, React.CSSProperties> = {
  'code[class*="language-"]': {
    color: "var(--color-text-primary)",
    background: "transparent",
    fontFamily: "var(--font-code)",
  },
  'pre[class*="language-"]': {
    color: "var(--color-text-primary)",
    background: "transparent",
    fontFamily: "var(--font-code)",
    margin: 0,
    padding: 0,
  },
  comment: { color: "var(--color-syntax-comment)", fontStyle: "italic" },
  prolog: { color: "var(--color-syntax-comment)" },
  doctype: { color: "var(--color-syntax-comment)" },
  cdata: { color: "var(--color-syntax-comment)" },
  punctuation: { color: "var(--color-syntax-punctuation)" },
  property: { color: "var(--color-syntax-builtin)" },
  tag: { color: "var(--color-syntax-keyword)" },
  boolean: { color: "var(--color-syntax-number)" },
  number: { color: "var(--color-syntax-number)" },
  constant: { color: "var(--color-syntax-number)" },
  symbol: { color: "var(--color-syntax-builtin)" },
  deleted: { color: "var(--color-danger)" },
  selector: { color: "var(--color-syntax-string)" },
  "attr-name": { color: "var(--color-syntax-builtin)" },
  string: { color: "var(--color-syntax-string)" },
  char: { color: "var(--color-syntax-string)" },
  builtin: { color: "var(--color-syntax-builtin)" },
  inserted: { color: "var(--color-success)" },
  operator: { color: "var(--color-syntax-operator)" },
  entity: { color: "var(--color-syntax-builtin)", cursor: "help" },
  url: { color: "var(--color-syntax-builtin)" },
  variable: { color: "var(--color-text-primary)" },
  atrule: { color: "var(--color-syntax-keyword)" },
  "attr-value": { color: "var(--color-syntax-string)" },
  function: { color: "var(--color-syntax-function)" },
  "class-name": { color: "var(--color-syntax-function)" },
  keyword: { color: "var(--color-syntax-keyword)" },
  regex: { color: "var(--color-syntax-number)" },
  important: { color: "var(--color-danger)", fontWeight: "bold" },
  bold: { fontWeight: "bold" },
  italic: { fontStyle: "italic" },
  decorator: { color: "var(--color-syntax-function)" },
  "triple-quoted-string": { color: "var(--color-syntax-string)" },
};

function ToolInputView({
  name,
  input,
  taskTrackerDisplay,
}: {
  name: string;
  input: string;
  taskTrackerDisplay: TaskTrackerDisplay | null;
}) {
  const parsed = parseJSON(input);
  if (!parsed || typeof parsed !== "object") {
    return <JsonFallback raw={input} />;
  }
  const args = parsed as Record<string, unknown>;

  if (name === "run_python" && typeof args.code === "string") {
    return <CodeBlock code={args.code} language="python" />;
  }

  if (name === "bash" && typeof args.command === "string") {
    const cwd = typeof args.working_dir === "string" ? args.working_dir : "";
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
          <span>bash</span>
          {cwd ? <span className="truncate normal-case tracking-normal text-[0.7rem]">cwd: {cwd}</span> : null}
        </div>
        <pre
          className="overflow-auto px-2 py-1.5 text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
          style={{ fontFamily: "var(--font-code)", maxHeight: "16rem" }}
        >
          <span className="select-none text-[var(--color-text-muted)]">$ </span>
          {args.command}
        </pre>
      </div>
    );
  }

  if (name === "task_tracker") {
    if (taskTrackerDisplay) {
      return <TaskList tasks={taskTrackerDisplay.tasks} />;
    }
    const cmd = typeof args.command === "string" ? args.command : "";
    if (cmd === "view") {
      return (
        <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] text-[var(--color-text-muted)]">
          viewing task list
        </div>
      );
    }
    const list = Array.isArray(args.task_list) ? (args.task_list as unknown[]) : [];
    if (cmd === "plan" && list.length > 0) {
      return <TaskList tasks={list} />;
    }
    return <JsonFallback raw={input} />;
  }

  if ((name === "view_file" || name === "write_file" || name === "edit_file") && typeof args.path === "string") {
    const content = typeof args.content === "string" ? args.content : "";
    const oldText = typeof args.old_text === "string" ? args.old_text : "";
    const newText = typeof args.new_text === "string" ? args.new_text : "";
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
          <span>{name.replace("_", " ")}</span>
          <span className="truncate normal-case tracking-normal text-[0.7rem] text-[var(--color-text-primary)]">{args.path}</span>
        </div>
        {name === "edit_file" && (oldText || newText) ? (
          <div className="grid gap-1 px-2 py-1.5 text-[0.72rem]" style={{ fontFamily: "var(--font-code)" }}>
            {oldText ? (
              <pre
                className="overflow-auto whitespace-pre-wrap"
                style={{ maxHeight: "8rem", color: "var(--color-danger)" }}
              >
                - {oldText}
              </pre>
            ) : null}
            {newText ? (
              <pre
                className="overflow-auto whitespace-pre-wrap"
                style={{ maxHeight: "8rem", color: "var(--color-success)" }}
              >
                + {newText}
              </pre>
            ) : null}
          </div>
        ) : name === "write_file" && content ? (
          <pre
            className="overflow-auto px-2 py-1.5 text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
            style={{ fontFamily: "var(--font-code)", maxHeight: "10rem" }}
          >
            {content}
          </pre>
        ) : null}
      </div>
    );
  }

  if (name === "web_fetch" && typeof args.url === "string") {
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] text-[var(--color-text-primary)]">
        <span className="text-[var(--color-text-muted)]">GET </span>
        <span style={{ fontFamily: "var(--font-code)" }}>{args.url}</span>
      </div>
    );
  }

  if (name === "smart_search" && typeof args.query === "string") {
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] text-[var(--color-text-primary)]">
        <span className="text-[var(--color-text-muted)]">search </span>
        <span className="italic">&ldquo;{args.query}&rdquo;</span>
      </div>
    );
  }

  return <JsonFallback raw={input} />;
}

// TaskList renders the task_tracker task array with status glyphs.
function TaskList({ tasks }: { tasks: unknown[] }) {
  return (
    <ul className="grid gap-0.5 rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.78rem]">
      {tasks.map((raw, i) => {
        const t = (raw ?? {}) as Record<string, unknown>;
        const title = typeof t.title === "string" ? t.title : "(untitled)";
        const status = typeof t.status === "string" ? t.status : "todo";
        const notes = typeof t.notes === "string" ? t.notes : "";
        const glyph =
          status === "done" ? "✓" : status === "in_progress" ? "◐" : "○";
        const style: React.CSSProperties =
          status === "done"
            ? {
                color: "var(--color-success)",
                textDecoration: "line-through",
                textDecorationColor: "color-mix(in srgb, var(--color-success) 40%, transparent)",
              }
            : status === "in_progress"
              ? { color: "var(--color-accent)" }
              : { color: "var(--color-text-primary)" };
        return (
          <li key={`${i}-${title}`} className="flex items-baseline gap-2">
            <span className="shrink-0 w-4 text-center" style={style} aria-hidden>
              {glyph}
            </span>
            <div className="min-w-0 flex-1">
              <div style={style}>{title}</div>
              {notes ? (
                <div className="text-[0.72rem] text-[var(--color-text-muted)]">{notes}</div>
              ) : null}
            </div>
          </li>
        );
      })}
    </ul>
  );
}

// ── Per-tool result renderers ────────────────────────────────────────────
//
// bash returns structured JSON (exit_code/stdout/stderr/...). Parse it
// and render a terminal-style block with an exit-code badge, matching
// the PythonOutput look so tool results feel consistent.
//
// task_tracker returns a summary + task list; render the list the same
// way we render its input.
//
// Everything else falls back to the raw result text in a monospace block.

function ToolResultView({
  name,
  resultText,
  isErr,
  taskTrackerDisplay,
}: {
  name: string;
  resultText: string;
  isErr: boolean;
  taskTrackerDisplay: TaskTrackerDisplay | null;
}) {
  if (name === "bash") {
    const parsed = parseJSON(resultText);
    if (parsed && typeof parsed === "object") {
      return <BashResult result={parsed as Record<string, unknown>} isErr={isErr} />;
    }
  }

  if (name === "task_tracker") {
    if (taskTrackerDisplay) {
      return (
        <TaskTrackerResult
          result={{
            tasks: taskTrackerDisplay.tasks,
            summary: taskTrackerDisplay.summary,
            active_task: taskTrackerDisplay.activeTask,
          }}
        />
      );
    }
    const parsed = parseJSON(resultText);
    if (parsed && typeof parsed === "object") {
      return <TaskTrackerResult result={parsed as Record<string, unknown>} />;
    }
  }

  return (
    <pre
      className="max-h-[16rem] overflow-auto rounded-[0.6rem] border bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] leading-[1.4]"
      style={{
        fontFamily: "var(--font-code)",
        borderColor: isErr ? "var(--color-danger-border)" : "var(--color-border)",
        color: isErr ? "var(--color-danger)" : "var(--color-text-secondary)",
      }}
    >
      {resultText}
    </pre>
  );
}

function BashResult({ result, isErr }: { result: Record<string, unknown>; isErr: boolean }) {
  const exitCode = typeof result.exit_code === "number" ? result.exit_code : -1;
  const stdout = typeof result.stdout === "string" ? result.stdout : "";
  const stderr = typeof result.stderr === "string" ? result.stderr : "";
  const elapsed = typeof result.execution_time_ms === "number" ? result.execution_time_ms : 0;
  const err = typeof result.error === "string" ? result.error : "";
  const failed = isErr || exitCode !== 0;

  return (
    <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
      <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem]">
        <span
          className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[0.62rem] font-medium uppercase tracking-wider"
          style={{
            borderColor: failed ? "var(--color-danger-border)" : "var(--color-success-border)",
            color: failed ? "var(--color-danger)" : "var(--color-success)",
          }}
        >
          exit {exitCode}
        </span>
        {elapsed ? (
          <span className="text-[var(--color-text-muted)]">{elapsed}ms</span>
        ) : null}
      </div>
      <div
        className="grid gap-1 px-2 py-1.5 text-[0.72rem] leading-[1.45]"
        style={{ fontFamily: "var(--font-code)" }}
      >
        {stdout ? (
          <pre
            className="overflow-auto whitespace-pre-wrap text-[var(--color-text-primary)]"
            style={{ maxHeight: "16rem" }}
          >
            {stdout}
          </pre>
        ) : null}
        {stderr ? (
          <pre
            className="overflow-auto whitespace-pre-wrap"
            style={{ maxHeight: "10rem", color: "var(--color-danger)" }}
          >
            {stderr}
          </pre>
        ) : null}
        {err ? (
          <p className="text-[0.7rem]" style={{ color: "var(--color-danger)" }}>
            {err}
          </p>
        ) : null}
        {!stdout && !stderr && !err ? (
          <p className="text-[0.7rem] text-[var(--color-text-muted)]">(no output)</p>
        ) : null}
      </div>
    </div>
  );
}

function TaskTrackerResult({ result }: { result: Record<string, unknown> }) {
  const tasks = Array.isArray(result.tasks) ? (result.tasks as unknown[]) : [];
  const summary = (result.summary ?? {}) as Record<string, unknown>;
  const total = typeof summary.total === "number" ? summary.total : tasks.length;
  const done = typeof summary.done === "number" ? summary.done : 0;
  const inProgress = typeof summary.in_progress === "number" ? summary.in_progress : 0;
  const todo = typeof summary.todo === "number" ? summary.todo : 0;

  return (
    <div className="grid gap-1.5">
      <div className="flex items-center gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
        <span>{total} total</span>
        {done ? (
          <span style={{ color: "var(--color-success)" }}>✓ {done} done</span>
        ) : null}
        {inProgress ? (
          <span style={{ color: "var(--color-accent)" }}>◐ {inProgress} in progress</span>
        ) : null}
        {todo ? <span>○ {todo} todo</span> : null}
      </div>
      {tasks.length > 0 ? <TaskList tasks={tasks} /> : null}
    </div>
  );
}

// ── Memory proposal card ─────────────────────────────────────────────────

function MemoryProposalCard({
  proposal,
  onResolved,
}: {
  proposal: MemoryProposal;
  onResolved: (next: MemoryProposal) => void;
}) {
  const [submitting, setSubmitting] = useState<"save" | "dismiss" | null>(null);

  const resolve = async (save: boolean) => {
    if (submitting || proposal.status !== "pending") return;
    setSubmitting(save ? "save" : "dismiss");
    try {
      if (save) {
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}/accept`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
        });
        if (!response.ok) {
          onResolved({ ...proposal, status: "dismissed" });
          return;
        }
        onResolved({ ...proposal, status: "saved" });
      } else {
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}`, {
          method: "DELETE",
        });
        if (!response.ok) {
          onResolved({ ...proposal, status: "dismissed" });
          return;
        }
        onResolved({ ...proposal, status: "dismissed" });
      }
    } catch {
      onResolved({ ...proposal, status: "dismissed" });
    } finally {
      setSubmitting(null);
    }
  };

  return (
    <div className="rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3">
      <div className="mb-2 flex items-center gap-2 text-[0.75rem] font-medium text-[var(--color-text-primary)]">
        <span className="inline-flex size-5 items-center justify-center rounded-full bg-[var(--color-accent)] text-[0.65rem] text-[var(--color-surface-1)]">
          M
        </span>
        Save this memory?
      </div>
      <p className="mb-3 whitespace-pre-wrap text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
        {proposal.content}
      </p>
      {proposal.status === "pending" ? (
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="rounded-full bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(true)}
          >
            {submitting === "save" ? "Saving..." : "Save"}
          </button>
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(false)}
          >
            {submitting === "dismiss" ? "Dismissing..." : "Don't save"}
          </button>
        </div>
      ) : (
        <div className="text-[0.75rem] text-[var(--color-text-muted)]">
          {proposal.status === "saved" ? "Saved to memories." : "Dismissed."}
        </div>
      )}
    </div>
  );
}
