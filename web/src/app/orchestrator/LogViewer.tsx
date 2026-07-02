"use client";

import { memo, useCallback, useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Task, TaskStreamFrame, TaskLearnedInstruction } from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { stripAnsiCodes } from "@/app/shared/lib/format";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { resolveTaskWorkspaceHref } from "@/app/chat/ui/workspaceHref";
import { Checklist, parseTaskTrackerOutput, type ChecklistState } from "./Checklist";

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
  // canStop shows the Stop button on a live run (#508). The server enforces
  // the real permission (admin); this only gates the affordance.
  canStop?: boolean;
};

export function LogViewer({ task, onClose, canStop }: LogViewerProps) {
  if (!task) return null;
  // Key the inner body on the task id so switching tasks remounts the fetch
  // hook — that reproduces the old "reset session to null then refetch on task
  // change" behavior cleanly, without a manual reset effect.
  const live = task.status === "running" || task.status === "assigned";
  if (live) {
    return <LiveTaskView key={task.id} task={task} onClose={onClose} canStop={!!canStop} />;
  }
  return <LogViewerBody key={task.id} task={task} onClose={onClose} canStop={!!canStop} />;
}

// ── #508 live activity view ──────────────────────────────────────────────────

type ActivityEntry = {
  key: string;
  kind: "message" | "tool_call" | "tool_result";
  name?: string;
  text: string;
  isError?: boolean;
  // checklist is set for a task_tracker tool_result whose JSON parsed into a
  // plan (#518); the entry then renders as a checklist instead of raw text.
  checklist?: ChecklistState;
};

const clampText = (s: string, max = 600) => (s.length > max ? s.slice(0, max) + "…" : s);

// LiveTaskView attaches to GET /tasks/{id}/stream and renders the run's
// tool-by-tool activity as it happens (#508): each tool call, its result, and
// the assistant's text, chronologically, with an optional Stop control that
// interrupts the governed run at its next checkpoint (with who-stopped-it
// attribution recorded server-side).
function LiveTaskView({ task, onClose, canStop }: { task: Task; onClose: () => void; canStop: boolean }) {
  const [entries, setEntries] = useState<ActivityEntry[]>([]);
  // checklist holds the LATEST task_tracker plan (#518) for the live progress
  // panel; it updates each time the agent revises its plan mid-run.
  const [checklist, setChecklist] = useState<ChecklistState | null>(null);
  const [runStatus, setRunStatus] = useState<string>("running");
  const [stoppedBy, setStoppedBy] = useState<string | null>(null);
  const [stopping, setStopping] = useState(false);
  const [streamError, setStreamError] = useState<string | null>(null);
  const seq = useRef(0);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    const onFrame = (frame: TaskStreamFrame) => {
      if (frame.type === "status") {
        if (frame.status && frame.status !== "running") {
          setRunStatus(frame.status);
          if (frame.stopped_by) setStoppedBy(frame.stopped_by);
        }
        return;
      }
      const key = `e${seq.current++}`;
      let entry: ActivityEntry | null = null;
      if (frame.type === "agent_message" && frame.content) {
        entry = { key, kind: "message", text: frame.content };
      } else if (frame.type === "tool_call") {
        entry = { key, kind: "tool_call", name: frame.name, text: clampText(frame.input ?? "") };
      } else if (frame.type === "tool_result") {
        // A task_tracker result carries the structured plan as JSON; parse it
        // (raw, before clamping) into a checklist for the live progress panel and
        // a readable history entry, falling back to raw text if it doesn't parse.
        const parsedChecklist =
          frame.name === "task_tracker" ? parseTaskTrackerOutput(frame.output ?? "") : null;
        if (parsedChecklist) {
          setChecklist(parsedChecklist);
          entry = { key, kind: "tool_result", name: frame.name, text: "", checklist: parsedChecklist };
        } else {
          entry = { key, kind: "tool_result", name: frame.name, text: clampText(frame.output ?? ""), isError: !!frame.error };
        }
      }
      if (entry) {
        setEntries((prev) => [...prev, entry]);
      }
    };
    orchestratorApi
      .streamTaskActivity(task.id, onFrame, ac.signal)
      .catch((err: unknown) => {
        if (!ac.signal.aborted) {
          setStreamError(err instanceof Error ? err.message : "stream failed");
        }
      });
    return () => ac.abort();
  }, [task.id]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [entries.length, runStatus]);

  const stop = async () => {
    if (stopping) return;
    setStopping(true);
    try {
      await orchestratorApi.cancelTask(task.id);
      // The terminal "stopped" frame arrives on the stream; nothing else to do.
    } catch (err) {
      setStreamError(err instanceof Error ? err.message : "stop failed");
      setStopping(false);
    }
  };

  const terminal = runStatus !== "running";
  return (
    <div className="modal-overlay is-open" role="dialog" aria-modal="true" aria-label="Live task activity">
      <div className="modal">
        <div className="modal-header">
          <h3>
            Live activity
            <span className={`status-badge status-${terminal ? (runStatus === "succeeded" ? "success" : runStatus === "stopped" ? "cancelled" : "error") : "running"}`} style={{ marginLeft: 8 }} data-testid="live-run-status">
              {runStatus}
              {stoppedBy ? ` by ${stoppedBy}` : ""}
            </span>
          </h3>
          <div style={{ display: "flex", gap: "0.5rem", alignItems: "center" }}>
            {canStop && !terminal ? (
              <button
                type="button"
                className="btn btn-danger"
                data-testid="stop-task-button"
                disabled={stopping}
                onClick={() => void stop()}
              >
                {stopping ? "Stopping…" : "Stop run"}
              </button>
            ) : null}
            <button type="button" className="icon-action modal-close" aria-label="Close modal" onClick={onClose}>
              ×
            </button>
          </div>
        </div>
        <div className="modal-body" data-testid="live-activity-body">
          {streamError ? <div className="table-error">Live stream error: {streamError}</div> : null}
          {checklist ? (
            <div className="live-checklist-panel">
              <Checklist state={checklist} live />
            </div>
          ) : null}
          {entries.length === 0 && !streamError ? (
            <div className="loading">
              <p>Waiting for activity…</p>
            </div>
          ) : (
            <div className="log-session" aria-live="polite">
              {entries.map((e) => (
                <div key={e.key} className={`log-message log-message--${e.kind === "message" ? "assistant" : "tool"}`}>
                  <div className="log-message-role">
                    {e.kind === "tool_call" ? `▶ ${e.name ?? "tool"}` : e.kind === "tool_result" ? `${e.isError ? "✗" : "✓"} ${e.name ?? "result"}` : "assistant"}
                  </div>
                  <div className="log-message-content">
                    {e.checklist ? (
                      <Checklist state={e.checklist} />
                    ) : (
                      <pre style={{ whiteSpace: "pre-wrap", margin: 0, fontFamily: e.kind === "message" ? "inherit" : undefined }}>{e.text}</pre>
                    )}
                  </div>
                </div>
              ))}
              <div ref={bottomRef} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function LogViewerBody({ task, onClose, canStop }: { task: Task; onClose: () => void; canStop: boolean }) {
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
        {task.expected_duration_minutes ? <SLADetail task={task} /> : null}
        <div className="modal-body" data-testid="log-modal-body">
          <SelfImprovePanel task={task} canManage={canStop} />
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

// SLADetail renders the task's actual vs. expected duration as a labeled
// progress bar (#274). Shown in the log modal header only when the task has an
// expected_duration_minutes. A breach (actual > expected * fail multiplier, or
// the latched sla_breached flag) turns the bar red; a warn crossing (actual >
// expected * warn multiplier) turns it amber; otherwise it stays green. The bar
// caps at 200% so a 10x overrun doesn't overflow the modal.
function SLADetail({ task }: { task: Task }) {
  const expected = task.expected_duration_minutes ?? 0;
  const actualSecs = task.actual_duration_seconds ?? 0;
  const actualMin = actualSecs / 60;
  const warnMul = task.sla_warn_multiplier || 1.5;
  const failMul = task.sla_fail_multiplier || 2.0;
  const warnMin = expected * warnMul;
  const failMin = expected * failMul;

  let tone = "ok";
  if (task.sla_breached || (expected > 0 && actualMin >= failMin)) tone = "fail";
  else if (expected > 0 && actualMin >= warnMin) tone = "warn";

  // Bar width: 100% == actual === expected. Cap at 200% so a runaway task
  // doesn't blow out the modal layout; the numeric label still shows the truth.
  const pct = expected > 0 ? Math.min((actualMin / expected) * 100, 200) : 0;
  const label = actualSecs > 0
    ? `${actualMin.toFixed(1)}m / ${expected}m expected`
    : `${expected}m expected (not yet complete)`;

  return (
    <div className="sla-detail" data-testid="sla-detail" data-sla-tone={tone}>
      <div className="sla-detail-label">
        <span>SLA</span>
        <span>{label}</span>
      </div>
      <div className="sla-progress" role="progressbar" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={200} aria-label="Actual vs expected duration">
        <div className={`sla-progress-bar sla-progress-${tone}`} style={{ width: `${pct}%` }} />
        <div className="sla-progress-mark sla-progress-mark-warn" style={{ left: `${Math.min(warnMul * 100, 200)}%` }} title={`warn @ ${warnMul}×`} />
        <div className="sla-progress-mark sla-progress-mark-fail" style={{ left: `${Math.min(failMul * 100, 200)}%` }} title={`fail @ ${failMul}×`} />
      </div>
    </div>
  );
}

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

// SelfImprovePanel (#516): thumbs up/down + critique on a finished task's
// output, and the versioned learned instructions distilled from that feedback
// — activate a version, or revert (deactivate). Operators (canManage) see the
// activate/revert controls; anyone who can view can leave feedback.
function SelfImprovePanel({ task, canManage }: { task: Task; canManage: boolean }) {
  const [instructions, setInstructions] = useState<TaskLearnedInstruction[]>([]);
  const [critique, setCritique] = useState("");
  const [note, setNote] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState(false);

  const reload = useCallback(async () => {
    try {
      const res = await orchestratorApi.learnedInstructions(task.id);
      setInstructions(res.learned_instructions ?? []);
    } catch {
      setInstructions([]);
    }
  }, [task.id]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void reload();
    });
    return () => {
      cancelled = true;
    };
  }, [open, reload]);

  const feedback = async (rating: "up" | "down") => {
    if (busy) return;
    setBusy(true);
    setNote(null);
    try {
      await orchestratorApi.submitFeedback(task.id, rating, rating === "down" ? critique : "");
      setCritique("");
      setNote(rating === "up" ? "Thanks — recorded." : "Recorded. Enough down-votes distills a learned instruction to review.");
      if (open) await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const activate = async (version: number) => {
    setBusy(true);
    try {
      await orchestratorApi.activateLearnedInstruction(task.id, version);
      await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const deactivate = async () => {
    setBusy(true);
    try {
      await orchestratorApi.deactivateLearnedInstruction(task.id);
      await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="self-improve-panel" data-testid="self-improve-panel" style={{ borderBottom: "1px solid var(--color-border)", paddingBottom: "0.75rem", marginBottom: "0.75rem" }}>
      <div style={{ display: "flex", alignItems: "center", gap: "0.5rem", flexWrap: "wrap" }}>
        <span style={{ fontSize: "0.8125rem", color: "var(--color-text-secondary)" }}>Was this run useful?</span>
        <button type="button" className="btn btn-small" disabled={busy} onClick={() => void feedback("up")}>👍</button>
        <button type="button" className="btn btn-small" disabled={busy} onClick={() => void feedback("down")}>👎</button>
        <input
          aria-label="Critique (optional)"
          placeholder="what to improve (optional)"
          value={critique}
          onChange={(e) => setCritique(e.target.value)}
          style={{ flex: "1 1 12rem", minWidth: 0, fontSize: "0.8rem", padding: "0.25rem 0.5rem", borderRadius: "0.5rem", border: "1px solid var(--color-border-strong)", background: "transparent", color: "var(--color-text-primary)" }}
        />
        <button type="button" className="btn btn-small" onClick={() => setOpen((v) => !v)}>
          {open ? "Hide" : "Learned instructions"}
        </button>
      </div>
      {note ? <div style={{ marginTop: "0.4rem", fontSize: "0.75rem", color: "var(--color-text-muted)" }}>{note}</div> : null}
      {open ? (
        instructions.length === 0 ? (
          <div style={{ marginTop: "0.5rem", fontSize: "0.78rem", color: "var(--color-text-muted)" }}>
            No learned instructions yet. Down-votes with critique distill into a reviewable instruction once self-improvement is enabled.
          </div>
        ) : (
          <ul style={{ marginTop: "0.5rem", display: "grid", gap: "0.35rem", listStyle: "none", padding: 0 }}>
            {instructions.map((li) => (
              <li key={li.id} style={{ fontSize: "0.78rem", color: "var(--color-text-secondary)", display: "flex", gap: "0.5rem", alignItems: "flex-start", justifyContent: "space-between" }}>
                <span>
                  <span className={`status-badge status-${li.status === "active" ? "success" : li.status === "proposed" ? "running" : "cancelled"}`} style={{ marginRight: 6 }}>
                    v{li.version} {li.status}
                  </span>
                  {li.content}
                </span>
                {canManage ? (
                  <span style={{ whiteSpace: "nowrap" }}>
                    {li.status !== "active" ? (
                      <button type="button" className="btn btn-small" disabled={busy} onClick={() => void activate(li.version)}>Activate</button>
                    ) : (
                      <button type="button" className="btn btn-small" disabled={busy} onClick={() => void deactivate()}>Deactivate</button>
                    )}
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        )
      ) : null}
    </div>
  );
}

export default LogViewer;
