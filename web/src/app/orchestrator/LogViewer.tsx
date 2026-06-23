"use client";

import { useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { stripAnsiCodes } from "@/app/shared/lib/format";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";

// LogViewer — the task log modal. React port of moc modals.js openLogModal().
// moc rendered logs with marked + DOMPurify + highlight.js; per the migration
// plan those are DROPPED in favor of react-markdown (the chat toolchain
// standard), which is safe-by-default (no raw HTML) so DOMPurify is unneeded.

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
                    <ReactMarkdown remarkPlugins={[remarkGfm]}>
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

export default LogViewer;
