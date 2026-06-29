"use client";

import { memo, useCallback, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { stripAnsiCodes } from "@/app/shared/lib/format";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { resolveTaskWorkspaceHref } from "@/app/chat/ui/workspaceHref";

// LogViewer — the task log modal. React port of moc modals.js openLogModal().
// moc rendered logs with marked + DOMPurify + highlight.js; per the migration
// plan those are DROPPED in favor of react-markdown (the chat toolchain
// standard), which is safe-by-default (no raw HTML) so DOMPurify is unneeded.
//
// Inline images (#271): a scheduled task's agent can produce an image with the
// generate_image tool and reference it in its reply as `![alt](weekly.png)`,
// exactly as it does in interactive chat. Without rewriting, ReactMarkdown
// would emit `<img src="weekly.png">` and the browser would 404 on a sibling of
// the orchestrator page. The img/a overrides below rewrite a RELATIVE workspace
// path to the task's workspace file proxy (resolveTaskWorkspaceHref →
// /api/orchestrator/tasks/<id>/workspace/<path>), reusing chat's existing
// workspace-href safety policy:
//   - Only relative paths the agent wrote into its own workspace are rewritten.
//     Absolute http(s)/data/mailto/protocol-relative/site-root hrefs pass
//     through untouched, so a poisoned log can't make the browser fetch an
//     arbitrary remote URL (no SSRF / tracking-pixel vector).
//   - The bytes are streamed through the authenticated, task-creator-scoped
//     workspace proxy (#287's GET /tasks/{id}/workspace/*), which path-guards
//     every access with SafeWorkspaceJoin.
//   - A workspace image that fails to load (file GC'd, still running, wrong
//     type) DEGRADES to a plain download link rather than a broken image.

export type LogViewerProps = {
  task: Task | null;
  onClose: () => void;
};

export function LogViewer({ task, onClose }: LogViewerProps) {
  if (!task) return null;
  // Key the inner body on the task id so switching tasks remounts the fetch
  // hook — that reproduces the old "reset session to null then refetch on task
  // change" behavior cleanly, without a manual reset effect.
  return <LogViewerBody key={task.id} task={task} onClose={onClose} />;
}

function LogViewerBody({ task, onClose }: { task: Task; onClose: () => void }) {
  // The shared hook owns the cancelled-ref guard and the lone setState after
  // the await, so this component no longer needs its own one-shot load-flag
  // setState-in-effect disable.
  const {
    data: session,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(() => orchestratorApi.taskLogs(task.id), [task.id]),
    [task.id],
  );

  return (
    <div className="modal-overlay is-open" role="dialog" aria-modal="true" aria-label="Task Logs">
      <div className="modal">
        <div className="modal-header">
          <h3>Task Logs</h3>
          <button type="button" className="icon-action modal-close" aria-label="Close modal" onClick={onClose}>
            ×
          </button>
        </div>
        <div className="modal-body" data-testid="log-modal-body">
          <TaskRunIfBanner task={task} />
          {loading ? (
            <div className="loading">
              <p>Loading logs...</p>
            </div>
          ) : error ? (
            <div className="table-error">Failed to load logs: {error}</div>
          ) : !session || !session.messages || session.messages.length === 0 ? (
            <div className="table-empty">No logs for this task.</div>
          ) : (
            <div className="log-session">
              {session.title ? <h4 className="log-session-title">{session.title}</h4> : null}
              {session.messages.map((msg, idx) => (
                <div key={msg.id ?? idx} className={`log-message log-message--${msg.role ?? "unknown"}`}>
                  <div className="log-message-role">{msg.role ?? "—"}</div>
                  <div className="log-message-content">
                    <ReactMarkdown
                      remarkPlugins={[remarkGfm]}
                      components={{
                        // Rewrite relative <img> srcs to the task workspace file
                        // proxy so agent-generated images render inline (#271).
                        // Absolute http(s)/data URLs pass through unchanged, so a
                        // log can't make the browser fetch an arbitrary remote URL.
                        img: ({ src, alt, title }) => {
                          const { href, isWorkspaceFile, downloadFilename } = resolveTaskWorkspaceHref(
                            typeof src === "string" ? src : "",
                            task.id,
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
                          const { href: resolved, isWorkspaceFile, downloadFilename } =
                            resolveTaskWorkspaceHref(typeof href === "string" ? href : "", task.id);
                          const isExternal = /^https?:\/\//i.test(resolved);
                          const extraProps: { target?: string; rel?: string; download?: string } = {};
                          if (isWorkspaceFile) {
                            extraProps.download = downloadFilename || "";
                          } else if (isExternal) {
                            extraProps.target = "_blank";
                            extraProps.rel = "noopener noreferrer";
                          }
                          return (
                            <a href={resolved || undefined} title={title ?? undefined} {...extraProps}>
                              {children}
                            </a>
                          );
                        },
                      }}
                    >
                      {stripAnsiCodes(msg.content ?? "")}
                    </ReactMarkdown>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
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

// TaskRunIfBanner renders the optional pre-run shell gate (#269) as a read-only
// code block + a collapsible skip badge. Shown at the top of the log modal so
// an operator sees the gate that gates this task and its recent skip history at
// a glance. Renders nothing when the task has no run_if.
function TaskRunIfBanner({ task }: { task: Task }) {
  const [expanded, setExpanded] = useState(false);
  if (!task.run_if) return null;
  const exitCode = task.run_if.exit_code_is ?? 0;
  const timeout = task.run_if.timeout_seconds ?? 30;
  const onError = task.run_if.on_error ?? "run";
  const skipped = (task.skip_count ?? 0) > 0;
  return (
    <div className="task-run-if-banner" data-testid="task-run-if-banner">
      <div className="task-run-if-banner__header">
        <span className="task-run-if-banner__title">Pre-run gate</span>
        <code className="task-run-if-banner__command">{task.run_if.command}</code>
        <span className="task-run-if-banner__meta">
          exit={exitCode} · timeout={timeout}s · on_error={onError}
        </span>
        {skipped ? (
          <button
            type="button"
            className="task-run-if-banner__skip-toggle"
            aria-expanded={expanded}
            aria-controls="task-run-if-skip-detail"
            onClick={() => setExpanded((v) => !v)}
          >
            Skipped {task.skip_count}×{expanded ? " ▾" : " ▸"}
          </button>
        ) : null}
      </div>
      {skipped && expanded ? (
        <div id="task-run-if-skip-detail" className="task-run-if-banner__skip-detail">
          <div>Last skip: {task.last_skip_at ? new Date(task.last_skip_at).toLocaleString() : "—"}</div>
          <div>Reason: {task.last_skip_reason ?? "—"}</div>
        </div>
      ) : null}
    </div>
  );
}

export default LogViewer;
