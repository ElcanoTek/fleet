"use client";

// Markdown renderer for the task-log modal — the ONLY orchestrator module
// that imports react-markdown. It is loaded lazily (React.lazy in
// LogViewer.tsx) so the ReactMarkdown pipeline (micromark + remark-gfm,
// ~43 KiB transfer) stays out of the initial /orchestrator bundle: nothing
// needs it until the user actually opens a task's log modal. Until the chunk
// arrives the modal shows the raw text with preserved whitespace, then
// upgrades in place. Same split as chat's AssistantContent (fleet #757).
//
// The img/a overrides are moved verbatim from LogViewer (#271): they rewrite
// RELATIVE workspace paths to the authenticated task workspace proxy while
// letting absolute http(s)/data hrefs pass through untouched, so a poisoned
// log can't make the browser fetch an arbitrary remote URL.

import { memo, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { resolveTaskWorkspaceHref } from "@/app/chat/ui/workspaceHref";

export default function LogMarkdown({
  content,
  taskId,
}: {
  content: string;
  taskId: string;
}) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        // Rewrite relative <img> srcs to the task workspace file
        // proxy so agent-generated images render inline (#271).
        // Absolute http(s)/data URLs pass through unchanged, so a
        // log can't make the browser fetch an arbitrary remote URL.
        img: ({ src, alt, title }) => {
          const { href, isWorkspaceFile, downloadFilename } =
            resolveTaskWorkspaceHref(
              typeof src === "string" ? src : "",
              taskId,
            );
          return (
            <LogImage
              src={href}
              alt={alt ?? ""}
              title={title ?? undefined}
              isWorkspaceFile={isWorkspaceFile}
              downloadFilename={downloadFilename}
            />
          );
        },
        // Same rewrite for <a href>: a link to a workspace file
        // (e.g. the agent links the image instead of embedding it)
        // gets a working href + a download attribute; external
        // links open in a new tab. Mirrors chat's anchor handling.
        a: ({ href, title, children }) => {
          const {
            href: resolved,
            isWorkspaceFile,
            downloadFilename,
          } = resolveTaskWorkspaceHref(
            typeof href === "string" ? href : "",
            taskId,
          );
          const isExternal = /^https?:\/\//i.test(resolved);
          const extraProps: {
            target?: string;
            rel?: string;
            download?: string;
          } = {};
          if (isWorkspaceFile) {
            extraProps.download = downloadFilename || "";
          } else if (isExternal) {
            extraProps.target = "_blank";
            extraProps.rel = "noopener noreferrer";
          }
          return (
            <a
              href={resolved || undefined}
              title={title ?? undefined}
              {...extraProps}
            >
              {children}
            </a>
          );
        },
      }}
    >
      {content}
    </ReactMarkdown>
  );
}

// LogImage renders an agent-produced image from a task's workspace, with a
// graceful fallback (#271). A workspace image that fails to load — the file was
// GC'd, the task is mid-run, or the referenced path isn't actually renderable —
// degrades to a plain download link (or, for a non-workspace href, the original
// reference) instead of a broken image icon. memo + eager/async decoding mirror
// chat's WorkspaceImage so the modal doesn't re-fetch on every parent render.
const LogImage = memo(function LogImage({
  src,
  alt,
  title,
  isWorkspaceFile,
  downloadFilename,
}: {
  src: string;
  alt: string;
  title?: string;
  isWorkspaceFile: boolean;
  downloadFilename: string;
}) {
  const [errored, setErrored] = useState(false);

  if (!src) {
    return <span className="log-image-fallback">{alt || "image"}</span>;
  }

  if (errored) {
    // Degrade to a link rather than a broken image. Workspace files get a
    // download attribute with the agent-chosen basename; everything else is a
    // bare link to the original reference.
    return (
      <a
        className="log-image-fallback"
        href={src}
        title={title}
        {...(isWorkspaceFile ? { download: downloadFilename || "" } : {})}
      >
        {alt || downloadFilename || "image (not available)"}
      </a>
    );
  }

  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      data-testid="log-image"
      className="log-image"
      src={src}
      alt={alt}
      title={title}
      loading="eager"
      decoding="async"
      onError={() => setErrored(true)}
    />
  );
});
