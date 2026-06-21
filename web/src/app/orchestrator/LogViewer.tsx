"use client";

import { useEffect, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { LogSession, Task } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { stripAnsiCodes } from "@/app/shared/lib/format";

// LogViewer — the task log modal. React port of moc modals.js openLogModal().
// moc rendered logs with marked + DOMPurify + highlight.js; per the migration
// plan those are DROPPED in favor of react-markdown (the chat toolchain
// standard), which is safe-by-default (no raw HTML) so DOMPurify is unneeded.

export type LogViewerProps = {
  task: Task | null;
  onClose: () => void;
};

export function LogViewer({ task, onClose }: LogViewerProps) {
  const [session, setSession] = useState<LogSession | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!task) return;
    let cancelled = false;
    /* eslint-disable react-hooks/set-state-in-effect -- one-shot load flags before fetch */
    setSession(null);
    setLoading(true);
    setError(null);
    /* eslint-enable react-hooks/set-state-in-effect */
    orchestratorApi
      .taskLogs(task.id)
      .then((s) => {
        if (!cancelled) setSession(s);
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [task]);

  if (!task) return null;

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
