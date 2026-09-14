"use client";

// GuideMarkdown — the user guides rendered as a document.
//
// Chat's AssistantContent and the orchestrator's LogMarkdown both render
// Markdown, but both are tuned for a dense transcript (tight margins, small
// type, a table that scrolls inside a bubble). A guide is read like a page, so
// it gets its own component and its own `.guide-prose` styles: real heading
// rhythm, anchored sections, and blockquotes that read as the callouts the
// guides use them for.
//
// Every h2/h3 carries the slug from headings.ts, which is what makes both the
// contents rail and the guides' own cross-references ("see [Run states]
// (#4-run-states)") land on the right section.

import type { ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { slugify } from "../headings";

// react-markdown hands heading children as nodes; the guides' headings are
// plain text, so flattening to a string is enough to slug them.
function textOf(children: ReactNode): string {
  if (typeof children === "string") return children;
  if (typeof children === "number") return String(children);
  if (Array.isArray(children)) return children.map(textOf).join("");
  return "";
}

export function GuideMarkdown({ content }: { content: string }) {
  return (
    <div className="guide-prose" data-testid="guide-prose">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          // h1 is the guide's own title, already rendered by the page header,
          // so the body's first line is dropped rather than repeated.
          h1: () => null,
          h2: ({ children }) => <h2 id={slugify(textOf(children))}>{children}</h2>,
          h3: ({ children }) => <h3 id={slugify(textOf(children))}>{children}</h3>,
          // Tables are the guides' workhorse (every control list is one) and
          // are the one element that can exceed a narrow viewport, so each gets
          // its own horizontal scroller instead of widening the page.
          table: ({ children }) => (
            <div className="guide-table-shell">
              <table>{children}</table>
            </div>
          ),
          a: ({ href, children }) => {
            const target = typeof href === "string" ? href : "";
            const external = /^https?:\/\//i.test(target);
            return (
              <a
                href={target}
                {...(external ? { target: "_blank", rel: "noreferrer noopener" } : {})}
              >
                {children}
              </a>
            );
          },
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
}

export default GuideMarkdown;
