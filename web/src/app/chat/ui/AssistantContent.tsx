"use client";

// Assistant markdown renderer, extracted verbatim from chat-experience.tsx
// (final planned slice of the #169 decomposition). This module owns the
// ReactMarkdown pipeline that turns an assistant/user message string into
// the chat transcript's rendered prose, plus its two private leaf
// components — WorkspaceImage and InlineHtmlPreview — which only the
// renderer mounts. No behavior, styling, or DOM changed in the move;
// chat-experience.tsx re-exports the public API so existing import paths
// (including the markdown unit tests) keep working.

import type { ReactElement, ReactNode } from "react";
import { Children, isValidElement, useContext, useMemo, useState } from "react";
import ReactMarkdown, { defaultUrlTransform, type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { CopyButton } from "./ChatChips";
import { DiffBlock } from "./DiffBlock";
import { isUnifiedDiff } from "@/app/lib/diffUtils";
import {
  decideReadOnlyFile,
  LOCKED_SUFFIX,
  normalizeAssistantMarkdown,
  PENDING_CONV_KEY,
  resolveWorkspaceHref,
  teamFileDownloadName,
  WITHHELD_SUFFIX,
  type ReadOnlyFileDecision,
} from "./workspaceHref";
// B19/B20 locked file names (the read-only views' render-time file policy +
// a branch's withheld files).
import {
  LockedFileLabel,
  ReadOnlyFilesContext,
  WithheldFileGate,
} from "./LockedFiles";
import { conversationWorkspaceUrl } from "@/app/lib/conversationApiUrl";
// WorkspaceImage moved to its own module so ToolChips can use it without
// statically importing this (now lazy-loaded) ReactMarkdown pipeline.
import { WorkspaceImage } from "./WorkspaceImage";
import {
  OutputShareMarker,
  useOutputShareMarkers,
  workspacePathFromHref,
} from "./OutputShareMarkers";

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
  return <AssistantMarkdown content={content} isStreaming={isStreaming} conversationId={conversationId} />;
}

export default function AssistantMarkdown({
  content,
  isStreaming = false,
  conversationId = null,
}: {
  content: string;
  isStreaming?: boolean;
  conversationId?: string | null;
}) {
  // Component functions are React element TYPES, not ordinary render callbacks.
  // Recreating them on a virtualizer measurement remounts every image/preview,
  // clearing failed-image state and causing another request + height change.
  // Keep their identity through parent rerenders and appended streaming text.
  const components = useMemo<Components>(() => ({
    h1: ({ children }) => (
      <h1 className="assistant-markdown-h1">{children}</h1>
    ),
    h2: ({ children }) => (
      <h2 className="assistant-markdown-h2">{children}</h2>
    ),
    h3: ({ children }) => (
      <h3 className="assistant-markdown-h3">{children}</h3>
    ),
    p: ({ children }) => <p className="assistant-markdown-p">{children}</p>,
    ul: ({ children }) => (
      <ul className="assistant-markdown-ul">{children}</ul>
    ),
    ol: ({ children }) => (
      <ol className="assistant-markdown-ol">{children}</ol>
    ),
    li: ({ children }) => (
      <li className="assistant-markdown-li">{children}</li>
    ),
    hr: () => <hr className="assistant-markdown-hr" />,
    table: ({ children }) => (
      <div className="assistant-markdown-table-shell">
        <table className="assistant-markdown-table">{children}</table>
      </div>
    ),
    thead: ({ children }) => (
      <thead className="assistant-markdown-thead">{children}</thead>
    ),
    th: ({ children }) => (
      <th className="assistant-markdown-th">{children}</th>
    ),
    td: ({ children }) => (
      <td className="assistant-markdown-td">{children}</td>
    ),
    code: ({ children, className }) => {
      const isBlock = Boolean(className);
      if (isBlock) {
        return (
          <code className="assistant-markdown-code-block">{children}</code>
        );
      }
      return (
        <code className="assistant-markdown-code-inline">{children}</code>
      );
    },
    pre: ({ children }) => {
      // Intercept ```html fences and render them as a sandboxed
      // preview so the agent can just emit HTML in a code block and
      // have it render. Anything else falls through to the default
      // <pre> styling, wrapped in a toolbar that exposes a copy
      // button and the language tag.
      //
      // A fenced code block renders <pre> with exactly one child element:
      // the inner <code>. Grab that single element child rather than
      // keying off a `language-*` className — react-markdown routes the
      // <code> through our own `code` override (so its `type` is that
      // override, not the string "code") and an untagged fence
      // (` ``` `…` ``` `) carries no language class at all. We still need
      // its text in both cases so isUnifiedDiff() can catch untagged diffs
      // the agent emits.
      const codeChild = Children.toArray(children).find((c) =>
        isValidElement(c),
      ) as
        | ReactElement<{ className?: string; children?: ReactNode }>
        | undefined;
      let language: string | null = null;
      let rawText = "";
      if (codeChild) {
        const cls = codeChild.props.className ?? "";
        const langMatch = cls.match(/language-([^\s]+)/i);
        if (langMatch) language = langMatch[1].toLowerCase();
        rawText =
          typeof codeChild.props.children === "string"
            ? codeChild.props.children
            : Children.toArray(codeChild.props.children).join("");
        if (language === "html") {
          return (
            <InlineHtmlPreview
              html={rawText.replace(/\n$/, "")}
              isStreaming={isStreaming}
              conversationId={conversationId}
            />
          );
        }
        // Render unified diffs as a coloured, gutter-marked diff view:
        // either an explicit ```diff / ```patch fence, or a bare code
        // block whose content matches the unified-diff shape (so agents
        // that forget the language tag still get highlighting). Everything
        // else falls through to the plain toolbar+<pre> path unchanged.
        if (
          language === "diff" ||
          language === "patch" ||
          isUnifiedDiff(rawText)
        ) {
          return <DiffBlock raw={rawText} />;
        }
      }
      const copyText = rawText.replace(/\n$/, "");
      return (
        <div className="assistant-markdown-pre-wrapper">
          <div className="assistant-markdown-pre-toolbar">
            <span className="assistant-markdown-pre-lang">
              {language ?? ""}
            </span>
            <CopyButton
              text={copyText}
              title="Copy code to clipboard"
              variant="compact"
            />
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
      const raw = typeof src === "string" ? src : "";
      const { href, downloadFilename: imageName } = resolveWorkspaceHref(
        raw,
        conversationId,
      );
      return (
        <ReadOnlyImageGate raw={raw} alt={alt ?? ""} title={title ?? undefined}>
          <WithheldFileGate href={href} name={imageName}>
            <WorkspaceImage
              key={href}
              src={href}
              alt={alt ?? ""}
              title={title ?? undefined}
            />
          </WithheldFileGate>
        </ReadOnlyImageGate>
      );
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
    a: ({ node, href, title, children }) => {
      const raw = typeof href === "string" ? href : "";
      // A read-only view (team or public) decides a workspace reference
      // here, on the href the CommonMark parser produced — see
      // ReadOnlyLinkGate. Every other chat renders the live link below.
      const innerImage = node?.children.find(
        (c) => c.type === "element" && c.tagName === "img",
      );
      const innerImageSrc =
        innerImage && innerImage.type === "element"
          ? String(innerImage.properties?.src ?? "")
          : null;
      return (
        <ReadOnlyLinkGate
          raw={raw}
          title={title ?? undefined}
          label={children}
          innerImageSrc={innerImageSrc}
        >
          {renderLiveLink(raw, title ?? undefined, children, conversationId)}
        </ReadOnlyLinkGate>
      );
    },
    strong: ({ children }) => (
      <strong className="assistant-markdown-strong">{children}</strong>
    ),
    em: ({ children }) => (
      <em className="assistant-markdown-em">{children}</em>
    ),
  }), [conversationId, isStreaming]);

  if (!content.trim()) {
    return null;
  }

  // The same pre-parse rewrites the read-only views apply before rewriting
  // file references (normalizeAssistantMarkdown), and that the server's output
  // discovery mirrors (outputs.go) — one definition, so the three agree on
  // what renders as a link.
  const normalizedContent = autoFenceRawHtmlDocument(
    normalizeAssistantMarkdown(content),
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
      urlTransform={(url) =>
        /^sandbox:/i.test(url) ? url : defaultUrlTransform(url)
      }
      components={components}
    >
      {normalizedContent}
    </ReactMarkdown>
  );
}

// renderLiveLink is the `a` override's ordinary rendering: rewrite a relative
// href to the per-conversation workspace API. When the agent writes
// `[Deck.pptx](Deck.pptx)` after producing the file via an MCP tool, the
// browser would otherwise try to navigate to a sibling path of the chat page
// and 404. Rewriting to the workspace API makes the link actually serve the
// file. Workspace links also get a `download` attribute so the browser saves
// the file instead of trying to render binary content inline, and external
// links open in a new tab so we don't lose the chat state. Visible styling
// (color + underline via .assistant-markdown-link) is what makes the link
// recognizable as a link at all — without it, react-markdown's bare <a>
// inherits body color and looks identical to surrounding text.
function renderLiveLink(
  raw: string,
  title: string | undefined,
  children: ReactNode,
  conversationId: string | null,
): ReactNode {
  const {
    href: resolved,
    isWorkspaceFile,
    downloadFilename,
  } = resolveWorkspaceHref(raw, conversationId);
  const isExternal = /^https?:\/\//i.test(resolved);
  const extraProps: {
    target?: string;
    rel?: string;
    download?: string;
  } = {};
  if (isWorkspaceFile) {
    // Pass the original basename so the browser saves with the
    // name the agent referenced, not a percent-encoded URL slice.
    extraProps.download = downloadFilename || "";
  } else if (teamFileDownloadName(resolved)) {
    // A team-files route: same save-don't-navigate treatment as the
    // owner's own workspace link.
    extraProps.download = teamFileDownloadName(resolved) ?? "";
  } else if (isExternal) {
    extraProps.target = "_blank";
    extraProps.rel = "noopener noreferrer";
  }
  const link = (
    <a
      className="assistant-markdown-link"
      href={resolved || undefined}
      title={title}
      {...extraProps}
    >
      {children}
    </a>
  );
  // B17: an owner's team-shared chat marks each output chip Shared /
  // Not shared. Only workspace files can be outputs; everything else —
  // and every chat with no marker context — renders unchanged.
  return isWorkspaceFile ? (
    <WithheldFileGate href={resolved} name={downloadFilename}>
      <OutputLinkWithMarker href={resolved} fallbackName={downloadFilename}>
        {link}
      </OutputLinkWithMarker>
    </WithheldFileGate>
  ) : (
    link
  );
}

// ── read-only views: files decided at render time ─────────────────────────
//
// Under a ReadOnlyFilesContext (ReadOnlyTranscript: a teammate's team view or
// a public share link) every workspace reference the markdown RENDERS is
// decided by its parsed href (decideReadOnlyFile): a shared output points at
// the team-files route; anything else renders as text — a locked name on the
// team view, a withheld name (or the image placeholder) on a public link —
// never an anchor or an <img>, because a disabled link is still a dead
// promise. Deciding here rather than by rewriting markdown source is what
// makes the answer agree with the server's goldmark-based output discovery:
// nested brackets, escaped `]`, balanced parens in a destination are all
// parsed by the same grammar on both sides.

/** The text a withheld/locked/upload decision renders as, or null when live. */
function readOnlyFileText(
  d: ReadOnlyFileDecision,
  imagePlaceholder: string | null,
): ReactNode | null {
  switch (d.kind) {
    case "locked":
      return <LockedFileLabel>{d.name + LOCKED_SUFFIX}</LockedFileLabel>;
    case "upload":
      return <span>{d.name}</span>;
    case "withheld":
      return (
        <span data-testid="withheld-file">
          {imagePlaceholder ?? d.name + WITHHELD_SUFFIX}
        </span>
      );
    default:
      return null;
  }
}

function ReadOnlyImageGate({
  raw,
  alt,
  title,
  children,
}: {
  raw: string;
  alt: string;
  title?: string;
  children: ReactNode;
}) {
  const policy = useContext(ReadOnlyFilesContext);
  if (!policy) return <>{children}</>;
  const d = decideReadOnlyFile(raw, policy);
  if (d.kind === "external") return <>{children}</>;
  if (d.kind === "shared") {
    return <WorkspaceImage key={d.url} src={d.url} alt={alt} title={title} />;
  }
  return readOnlyFileText(
    d,
    policy.mode === "withhold" ? policy.imagePlaceholder : null,
  );
}

function ReadOnlyLinkGate({
  raw,
  title,
  label,
  innerImageSrc,
  children,
}: {
  raw: string;
  title?: string;
  label: ReactNode;
  /** The src of an image inside this link (a clickable thumbnail), if any. */
  innerImageSrc: string | null;
  children: ReactNode;
}) {
  const policy = useContext(ReadOnlyFilesContext);
  if (!policy) return <>{children}</>;
  const d = decideReadOnlyFile(raw, policy);
  if (d.kind === "external") return <>{children}</>;
  if (d.kind === "shared") {
    // A thumbnail whose image is NOT live (withheld or locked) is labelled
    // by the file it downloads, beside the image's own locked name — never a
    // live link whose only visible text says "not shared".
    const innerLive =
      innerImageSrc === null ||
      ["external", "shared"].includes(decideReadOnlyFile(innerImageSrc, policy).kind);
    const anchor = (text: ReactNode) => (
      <a
        className="assistant-markdown-link"
        href={d.url}
        title={title}
        download={d.name}
      >
        {text}
      </a>
    );
    return innerLive ? (
      anchor(label)
    ) : (
      <>
        {label} {anchor(d.name)}
      </>
    );
  }
  const text = readOnlyFileText(d, null);
  // A clickable thumbnail whose target is withheld: on the team view the
  // image (live if it was shared, else its own locked name) stays beside the
  // target's locked name — one name when both are the same file. On a public
  // link the target's withheld name stands alone, as the source pre-pass
  // renders it.
  if (innerImageSrc !== null && policy.mode === "shared") {
    const inner = decideReadOnlyFile(innerImageSrc, policy);
    if (inner.kind !== "external" && inner.kind !== "shared" && inner.path === d.path) {
      return <>{label}</>;
    }
    return (
      <>
        {label} {text}
      </>
    );
  }
  return <>{text}</>;
}

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
function InlineHtmlPreview({
  html,
  isStreaming = false,
  conversationId,
}: {
  html: string;
  isStreaming?: boolean;
  conversationId?: string | null;
}) {
  // Inject a <base> tag so relative image/link paths in the LLM-generated HTML
  // resolve to the workspace API. This allows charts and other files generated
  // by the agent to render correctly inside the sandboxed iframe.
  let processedHtml = html;
  const baseHref =
    conversationId && conversationId !== PENDING_CONV_KEY
      ? conversationWorkspaceUrl(conversationId)
      : null;
  if (baseHref) {
    const baseTag = `<base href="${baseHref}">`;
    if (/<head[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(/(<head[^>]*>)/i, `$1\n${baseTag}`);
    } else if (/<html[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(
        /(<html[^>]*>)/i,
        `$1\n<head>\n${baseTag}\n</head>`,
      );
    } else if (/<!DOCTYPE[^>]*>/i.test(processedHtml)) {
      processedHtml = processedHtml.replace(
        /(<!DOCTYPE[^>]*>)/i,
        `$1\n<head>\n${baseTag}\n</head>`,
      );
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
        <span>
          Building HTML preview ({html.length.toLocaleString()} chars so far)…
        </span>
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


// OutputLinkWithMarker appends the B17 share marker to a workspace link when
// the transcript sits under an OutputShareContext (an owner's team-shared
// chat) and the link's path is one of that chat's outputs. It reads the
// context itself so the memoised `components` map keeps its identity.
function OutputLinkWithMarker({
  href,
  fallbackName,
  children,
}: {
  href: string;
  fallbackName: string;
  children: ReactNode;
}) {
  const markers = useOutputShareMarkers();
  if (!markers) return <>{children}</>;
  const path = workspacePathFromHref(href, markers.conversationId);
  const shared = path === null ? undefined : markers.shared.get(path);
  if (path === null || shared === undefined) return <>{children}</>;
  return (
    <>
      {children}
      <OutputShareMarker
        name={path.split("/").pop() || fallbackName}
        shared={shared}
        team={markers.team}
        onClick={() => markers.onOpenSources(path)}
      />
    </>
  );
}
