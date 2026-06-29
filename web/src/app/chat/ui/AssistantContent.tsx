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
import { Children, isValidElement, memo, useState } from "react";
import ReactMarkdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
import { CopyButton } from "./ChatChips";
import { DiffBlock } from "./DiffBlock";
import { isUnifiedDiff } from "@/app/lib/diffUtils";
import { PENDING_CONV_KEY, resolveWorkspaceHref } from "./workspaceHref";

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
          //
          // A fenced code block renders <pre> with exactly one child element:
          // the inner <code>. Grab that single element child rather than
          // keying off a `language-*` className — react-markdown routes the
          // <code> through our own `code` override (so its `type` is that
          // override, not the string "code") and an untagged fence
          // (` ``` `…` ``` `) carries no language class at all. We still need
          // its text in both cases so isUnifiedDiff() can catch untagged diffs
          // the agent emits.
          const codeChild = Children.toArray(children).find((c) => isValidElement(c)) as
            | ReactElement<{ className?: string; children?: ReactNode }>
            | undefined;
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
            // Render unified diffs as a coloured, gutter-marked diff view:
            // either an explicit ```diff / ```patch fence, or a bare code
            // block whose content matches the unified-diff shape (so agents
            // that forget the language tag still get highlighting). Everything
            // else falls through to the plain toolbar+<pre> path unchanged.
            if (language === "diff" || language === "patch" || isUnifiedDiff(rawText)) {
              return <DiffBlock raw={rawText} />;
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
export const WorkspaceImage = memo(function WorkspaceImage({
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
