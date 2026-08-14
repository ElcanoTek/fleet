"use client";

// Tool-call rendering family extracted from chat-experience.tsx (slice 3 of
// #169). These components turn an agent ToolCall (and run_python output) into
// the collapsible chips and per-tool input/result views shown under an
// assistant turn. The whole family is self-contained: it depends only on the
// shared history.ts helpers/types, the byte formatters, and the syntax
// highlighter — never on the ChatExperience component's mutable state. Moving
// it here is a pure relocation; behavior, markup, and class names are
// unchanged.

import { lazy, Suspense, useState } from "react";
import {
  prettyToolName,
  safePretty,
  toolIcon,
  type Message,
  type PythonStream,
  type ToolCall,
} from "./history";
// From its own module (not AssistantContent) so this file doesn't statically
// pull the lazy-loaded ReactMarkdown pipeline back into the initial bundle.
import { WorkspaceImage } from "./WorkspaceImage";
import { resolveWorkspaceHref } from "./workspaceHref";

// The syntax highlighter (react-syntax-highlighter + grammars, ~75 KiB
// transfer) is the single largest dependency of the initial /chat bundle,
// but nothing renders it until the user EXPANDS a tool chip — every chip
// starts collapsed. React.lazy defers the whole module to a separate chunk
// fetched on first expand; until it arrives CodeBlock shows the same plain
// <pre> it already uses for unsupported languages, so the swap is invisible
// except for the (one-time, sub-second) upgrade to token colors.
const HighlightedCode = lazy(() => import("./CodeHighlight"));

// ── Python output block ──────────────────────────────────────────────────
//
// Renders a run_python result as terminal-style output — stdout in the
// default monospace color, stderr tinted red. Empty output is suppressed
// so we don't render an empty black box. Execution time (when the bridge
// reports it) is shown as a small footer.

export function PythonOutput({
  stream,
  conversationId,
}: {
  stream: PythonStream;
  conversationId?: string | null;
}) {
  const stdout = stream.stdout ?? "";
  const stderr = stream.stderr ?? "";
  const error = stream.error ?? "";
  const hasErr = Boolean(stderr.trim() || error.trim());
  const images = stream.imageFiles ?? [];
  // Always start collapsed. Line count is a poor signal for "is this
  // small enough to inline" — a single line can be a 5000-char pandas
  // repr that bleeds across the chat column on mobile. User taps the
  // header to reveal.
  const [expanded, setExpanded] = useState(false);
  // If everything is blank, skip the block entirely. Placed AFTER
  // the useState call so React sees hooks in the same order every
  // render (rules-of-hooks). Figures count as content — a chart-only
  // cell (no stdout) must still render its figure (#213).
  if (!stdout.trim() && !hasErr && !stream.executionMs && images.length === 0) {
    return null;
  }
  // Figures (matplotlib etc.) render ALWAYS-visible — the whole point is the
  // user sees the chart inline without expanding anything. Each relative path
  // is resolved to the authenticated per-conversation workspace URL, the same
  // proxy that serves ![](chart.png) markdown images.
  const figures =
    images.length > 0 ? (
      <div className="grid gap-1.5">
        {images.map((path, i) => {
          const { href } = resolveWorkspaceHref(path, conversationId ?? null);
          return (
            <WorkspaceImage key={`${path}-${i}`} src={href} alt="figure" />
          );
        })}
      </div>
    ) : null;
  // A figure-only cell (no terminal output / timing) renders just the figures —
  // no empty collapsible "python output" box.
  if (!stdout.trim() && !hasErr && !stream.executionMs) {
    return figures;
  }
  const stdoutLines = stdout ? stdout.split("\n").length : 0;
  const summaryBits = [
    stdoutLines ? `${stdoutLines} line${stdoutLines === 1 ? "" : "s"}` : "",
    stream.executionMs ? `${stream.executionMs}ms` : "",
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <div className="grid min-w-0 gap-1.5">
      {figures}
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
              <span
                className="rounded-full border px-1.5 text-[0.62rem]"
                style={{
                  borderColor: "var(--color-danger-border)",
                  color: "var(--color-danger)",
                }}
              >
                error
              </span>
            ) : null}
          </span>
          <span className="text-[var(--color-text-muted)]">
            {expanded ? "collapse" : "expand"}
          </span>
        </button>
        {expanded ? (
          <div
            className="border-t border-[var(--color-border)] px-3 py-2 text-[0.78rem] leading-[1.55]"
            style={{ fontFamily: "var(--font-code)" }}
          >
            {stdout ? (
              <pre className="overflow-x-auto whitespace-pre-wrap text-[var(--color-text-primary)]">
                {stdout}
              </pre>
            ) : null}
            {stderr ? (
              <pre
                className="mt-1 overflow-x-auto whitespace-pre-wrap"
                style={{ color: "var(--color-danger)" }}
              >
                {stderr}
              </pre>
            ) : null}
            {error ? (
              <pre
                className="mt-1 overflow-x-auto whitespace-pre-wrap"
                style={{ color: "var(--color-danger)" }}
              >
                {error}
              </pre>
            ) : null}
          </div>
        ) : null}
      </div>
    </div>
  );
}

// ── Tool chip ────────────────────────────────────────────────────────────

export function ToolChip({
  tc,
  taskTrackerDisplay,
}: {
  tc: ToolCall;
  taskTrackerDisplay: TaskTrackerDisplay | null;
}) {
  const [open, setOpen] = useState(false);
  const label = prettyToolName(tc.name);
  const stateStyle: React.CSSProperties =
    tc.state === "error"
      ? {
          borderColor: "var(--color-danger-border)",
          color: "var(--color-danger)",
        }
      : tc.state === "pending"
        ? {
            borderColor: "var(--color-border-strong)",
            color: "var(--color-text-muted)",
          }
        : {
            borderColor: "var(--color-border-strong)",
            color: "var(--color-text-secondary)",
          };

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
            <ToolInputView
              name={tc.name}
              input={tc.input}
              taskTrackerDisplay={taskTrackerDisplay}
            />
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
  const inputTasks = Array.isArray(obj.task_list)
    ? (obj.task_list as unknown[])
    : [];
  const source = resultTasks.length > 0 ? resultTasks : inputTasks;
  return source.flatMap((entry) => {
    const task = (entry ?? {}) as Record<string, unknown>;
    if (typeof task.title !== "string" || !task.title.trim()) return [];
    const status = task.status;
    if (status !== "todo" && status !== "in_progress" && status !== "done")
      return [];
    return [
      {
        title: task.title.trim(),
        status,
        notes:
          typeof task.notes === "string" && task.notes.trim()
            ? task.notes.trim()
            : undefined,
      } satisfies DisplayTask,
    ];
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

export function taskTrackerDisplayForMessage(
  message: Message,
): TaskTrackerDisplay | null {
  let tracker: ToolCall | null = null;
  const tc = message.toolCalls ?? [];
  for (let i = tc.length - 1; i >= 0; i--) {
    if (tc[i].name === "task_tracker") {
      tracker = tc[i];
      break;
    }
  }
  if (!tracker) return null;
  const baseTasks = tracker.resultText
    ? parseTaskList(tracker.resultText)
    : parseTaskList(tracker.input);
  if (baseTasks.length === 0) return null;

  const activeTask =
    baseTasks.find((task) => task.status === "in_progress")?.title ??
    baseTasks.find((task) => task.status !== "done")?.title ??
    "";

  return {
    tasks: baseTasks,
    summary: summarizeDisplayTasks(baseTasks),
    activeTask,
  };
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
          <Suspense fallback={<PlainCode code={code} />}>
            <HighlightedCode code={code} language={language} />
          </Suspense>
        ) : (
          <PlainCode code={code} />
        )}
      </div>
    </div>
  );
}

// PlainCode is CodeBlock's unhighlighted body: used directly for languages
// without a registered grammar, and as the Suspense fallback while the lazy
// CodeHighlight chunk loads. Identical type metrics to the highlighted
// rendering so the upgrade doesn't shift layout.
function PlainCode({ code }: { code: string }) {
  return (
    <pre
      className="text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
      style={{ fontFamily: "var(--font-code)" }}
    >
      {code}
    </pre>
  );
}

// Languages with a grammar registered in CodeHighlight.tsx — keep the two in
// sync. The set lives HERE (not in CodeHighlight) so gating on it doesn't
// statically pull the lazy highlighter module back into the initial bundle.
const syntaxSupportedLanguages = new Set([
  "python",
  "bash",
  "shell",
  "json",
  "yaml",
]);

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
          {cwd ? (
            <span className="truncate normal-case tracking-normal text-[0.7rem]">
              cwd: {cwd}
            </span>
          ) : null}
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
    const list = Array.isArray(args.task_list)
      ? (args.task_list as unknown[])
      : [];
    if (cmd === "plan" && list.length > 0) {
      return <TaskList tasks={list} />;
    }
    return <JsonFallback raw={input} />;
  }

  if (
    (name === "view_file" || name === "write_file" || name === "edit_file") &&
    typeof args.path === "string"
  ) {
    const content = typeof args.content === "string" ? args.content : "";
    const oldText = typeof args.old_text === "string" ? args.old_text : "";
    const newText = typeof args.new_text === "string" ? args.new_text : "";
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
          <span>{name.replace("_", " ")}</span>
          <span className="truncate normal-case tracking-normal text-[0.7rem] text-[var(--color-text-primary)]">
            {args.path}
          </span>
        </div>
        {name === "edit_file" && (oldText || newText) ? (
          <div
            className="grid gap-1 px-2 py-1.5 text-[0.72rem]"
            style={{ fontFamily: "var(--font-code)" }}
          >
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

  // Governed sub-agent delegation (#1043): show the child's role + task, not
  // raw JSON.
  if (name === "spawn_subagent" && typeof args.task === "string") {
    const role = typeof args.role === "string" && args.role ? args.role : "explore";
    return (
      <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
        <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wider text-[var(--color-text-muted)]">
          <span>sub-agent</span>
          <span className="rounded-full border border-[var(--color-border)] px-1.5 normal-case tracking-normal">
            {role}
          </span>
        </div>
        <pre
          className="overflow-auto whitespace-pre-wrap px-2 py-1.5 text-[0.72rem] leading-[1.4] text-[var(--color-text-primary)]"
          style={{ maxHeight: "10rem" }}
        >
          {args.task}
        </pre>
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
                textDecorationColor:
                  "color-mix(in srgb, var(--color-success) 40%, transparent)",
              }
            : status === "in_progress"
              ? { color: "var(--color-accent)" }
              : { color: "var(--color-text-primary)" };
        return (
          <li key={`${i}-${title}`} className="flex items-baseline gap-2">
            <span
              className="shrink-0 w-4 text-center"
              style={style}
              aria-hidden
            >
              {glyph}
            </span>
            <div className="min-w-0 flex-1">
              <div style={style}>{title}</div>
              {notes ? (
                <div className="text-[0.72rem] text-[var(--color-text-muted)]">
                  {notes}
                </div>
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
// send_email (built-in and the MCP variants) returns a provider payload —
// status_code / message_id / validation_warnings, or an `error` key on
// failure. Dumping that raw put a wall of JSON and HTML-lint prose at the
// end of the transcript, right where the user had just clicked Send; render
// the outcome as one line instead and keep the payload behind a disclosure.
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
      return (
        <BashResult result={parsed as Record<string, unknown>} isErr={isErr} />
      );
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

  if (name === "spawn_subagent") {
    const parsed = parseJSON(resultText);
    if (parsed && typeof parsed === "object") {
      return (
        <SubagentSpawnResult result={parsed as Record<string, unknown>} />
      );
    }
  }

  if (isEmailSendTool(name)) {
    // The approval gate resolves this call twice: first with a plain-text
    // APPROVAL_REQUIRED placeholder (is_err), then — after the user clicks
    // Send — with the provider's JSON. Both reach the transcript, so both
    // get a human-readable form.
    if (resultText.startsWith(APPROVAL_REQUIRED_PREFIX)) {
      return <EmailSendPending />;
    }
    const parsed = parseJSON(resultText);
    if (parsed && typeof parsed === "object") {
      return (
        <EmailSendResult
          result={parsed as Record<string, unknown>}
          isErr={isErr}
          raw={resultText}
        />
      );
    }
  }

  return (
    <pre
      className="max-h-[16rem] overflow-auto rounded-[0.6rem] border bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] leading-[1.4]"
      style={{
        fontFamily: "var(--font-code)",
        borderColor: isErr
          ? "var(--color-danger-border)"
          : "var(--color-border)",
        color: isErr ? "var(--color-danger)" : "var(--color-text-secondary)",
      }}
    >
      {resultText}
    </pre>
  );
}

function BashResult({
  result,
  isErr,
}: {
  result: Record<string, unknown>;
  isErr: boolean;
}) {
  const exitCode = typeof result.exit_code === "number" ? result.exit_code : -1;
  const stdout = typeof result.stdout === "string" ? result.stdout : "";
  const stderr = typeof result.stderr === "string" ? result.stderr : "";
  const elapsed =
    typeof result.execution_time_ms === "number" ? result.execution_time_ms : 0;
  const err = typeof result.error === "string" ? result.error : "";
  const failed = isErr || exitCode !== 0;

  return (
    <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
      <div className="flex items-center gap-2 border-b border-[var(--color-border)] px-2 py-0.5 text-[0.65rem]">
        <span
          className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[0.62rem] font-medium uppercase tracking-wider"
          style={{
            borderColor: failed
              ? "var(--color-danger-border)"
              : "var(--color-success-border)",
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
          <p className="text-[0.7rem] text-[var(--color-text-muted)]">
            (no output)
          </p>
        ) : null}
      </div>
    </div>
  );
}

// ── send_email result ────────────────────────────────────────────────────
//
// Matches the built-in `send_email` and every MCP variant a bundle exposes
// (`mcp_sendgrid_send_email`, `mcp_mailbux_send_email`, …) — a suffix match,
// so `preview_email` and other email-adjacent tools keep the raw view.

const APPROVAL_REQUIRED_PREFIX = "APPROVAL_REQUIRED";

export function isEmailSendTool(name: string): boolean {
  return name === "send_email" || name.endsWith("_send_email");
}

/** One validation warning from the provider's HTML pre-flight. */
type EmailWarning = { rule: string; severity: string; message: string; hint: string };

function readWarnings(value: unknown): EmailWarning[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((entry) => {
    if (!entry || typeof entry !== "object") return [];
    const w = entry as Record<string, unknown>;
    const message = typeof w.message === "string" ? w.message : "";
    if (!message) return [];
    return [
      {
        rule: typeof w.rule === "string" ? w.rule : "",
        severity: typeof w.severity === "string" ? w.severity : "warning",
        message,
        hint: typeof w.hint === "string" ? w.hint : "",
      },
    ];
  });
}

// SubagentSpawnResult renders a spawn_subagent JSON result (#1043) as a child
// card — status, role, spend — with the child's answer behind a disclosure,
// never a raw JSON blob.
function SubagentSpawnResult({ result }: { result: Record<string, unknown> }) {
  const success = result.success === true;
  const role = typeof result.role === "string" ? result.role : "";
  const childId =
    typeof result.child_session_id === "string" ? result.child_session_id : "";
  const refused = !success && !childId; // a refusal never built a child
  const cost = typeof result.cost_usd === "number" ? result.cost_usd : 0;
  const tokens = typeof result.tokens === "number" ? result.tokens : 0;
  const answer = typeof result.result === "string" ? result.result : "";
  const status = success ? "done" : refused ? "refused" : "failed";
  return (
    <div
      className="rounded-[0.6rem] border bg-[var(--color-overlay-strong)] min-w-0 max-w-full"
      style={{
        borderColor: success ? "var(--color-border)" : "var(--color-danger-border)",
      }}
      data-testid="chat-subagent-card"
    >
      <div className="flex items-center gap-2 px-2 py-1 text-[0.7rem]">
        <span className="font-semibold text-[var(--color-text-primary)]">
          Sub-agent{childId ? ` ${childId.replace(/^subagent-/, "").slice(0, 8)}…` : ""}
        </span>
        {role ? (
          <span className="rounded-full border border-[var(--color-border)] px-1.5 text-[var(--color-text-muted)]">
            {role}
          </span>
        ) : null}
        <span
          className="ml-auto font-semibold"
          style={{ color: success ? "var(--color-success)" : "var(--color-danger)" }}
        >
          {status}
        </span>
      </div>
      <div className="flex flex-wrap gap-2 px-2 pb-1 text-[0.68rem] text-[var(--color-text-muted)]">
        {cost > 0 ? <span>${cost.toFixed(4)}</span> : null}
        {tokens > 0 ? <span>{tokens.toLocaleString()} tokens</span> : null}
      </div>
      {answer ? (
        <details className="border-t border-[var(--color-border)] px-2 py-1 text-[0.72rem]">
          <summary className="cursor-pointer text-[var(--color-text-muted)]">
            Result
          </summary>
          <pre
            className="overflow-auto whitespace-pre-wrap py-1 leading-[1.4] text-[var(--color-text-secondary)]"
            style={{ maxHeight: "12rem" }}
          >
            {answer}
          </pre>
        </details>
      ) : null}
    </div>
  );
}

function EmailSendPending() {
  return (
    <p className="text-[0.72rem] text-[var(--color-text-muted)]">
      Waiting for your approval — the email is staged and nothing has been sent
      yet.
    </p>
  );
}

function EmailSendResult({
  result,
  isErr,
  raw,
}: {
  result: Record<string, unknown>;
  isErr: boolean;
  raw: string;
}) {
  const error = typeof result.error === "string" ? result.error.trim() : "";
  const statusCode =
    typeof result.status_code === "number" ? result.status_code : null;
  const messageId =
    typeof result.message_id === "string" ? result.message_id.trim() : "";
  const note = typeof result.note === "string" ? result.note.trim() : "";
  const duplicate = result.duplicate_suppressed === true;
  const warnings = readWarnings(result.validation_warnings);

  // The server reports a rejected send as a payload with an `error` key and a
  // 2xx-less status — NOT as a tool-level error — so the status must be read
  // off the payload. Trusting is_err alone is what would print "queued" over
  // a failed send.
  const httpFailed = statusCode !== null && (statusCode < 200 || statusCode >= 300);
  const failed = isErr || error !== "" || httpFailed;

  const headline = failed
    ? "Not sent"
    : duplicate
      ? "Already sent"
      : "Queued for delivery";
  const detail = failed
    ? error ||
      (statusCode !== null
        ? `the email provider returned status ${statusCode}`
        : "the email provider rejected the send")
    : duplicate
      ? note ||
        "an identical email was already sent by an earlier run of this task"
      : "";

  return (
    <div className="rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] min-w-0 max-w-full">
      <div className="flex flex-wrap items-center gap-2 px-2 py-1.5 text-[0.72rem]">
        <span
          className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[0.62rem] font-medium uppercase tracking-wider"
          style={{
            borderColor: failed
              ? "var(--color-danger-border)"
              : "var(--color-success-border)",
            color: failed ? "var(--color-danger)" : "var(--color-success)",
          }}
        >
          {headline}
        </span>
        {detail ? (
          <span
            data-testid="email-send-detail"
            className="min-w-0 flex-1"
            style={{
              color: failed
                ? "var(--color-danger)"
                : "var(--color-text-secondary)",
            }}
          >
            {detail}
          </span>
        ) : null}
        {!failed && warnings.length ? (
          <span
            className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[0.62rem]"
            style={{
              borderColor: "var(--color-warning-border)",
              color: "var(--color-warning)",
            }}
          >
            {warnings.length === 1
              ? "1 formatting note"
              : `${warnings.length} formatting notes`}
          </span>
        ) : null}
      </div>
      <details className="border-t border-[var(--color-border)]">
        <summary className="cursor-pointer px-2 py-1 text-[0.68rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)]">
          Delivery details
        </summary>
        <div className="grid gap-1.5 px-2 pb-2 text-[0.7rem] leading-[1.45]">
          {messageId ? (
            <p className="text-[var(--color-text-secondary)]">
              Message ID:{" "}
              <span style={{ fontFamily: "var(--font-code)" }}>{messageId}</span>
            </p>
          ) : null}
          {warnings.length ? (
            <ul className="grid gap-1">
              {warnings.map((w, i) => (
                <li
                  key={`${w.rule}-${i}`}
                  className="text-[var(--color-text-secondary)]"
                >
                  <span style={{ color: "var(--color-warning)" }}>
                    {w.severity}
                    {w.rule ? ` ${w.rule}` : ""}
                  </span>{" "}
                  — {w.message}
                  {w.hint ? (
                    <span className="text-[var(--color-text-muted)]">
                      {" "}
                      {w.hint}
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          ) : null}
          <pre
            className="max-h-[16rem] overflow-auto rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1.5 text-[0.68rem] leading-[1.4] text-[var(--color-text-muted)]"
            style={{ fontFamily: "var(--font-code)" }}
          >
            {raw}
          </pre>
        </div>
      </details>
    </div>
  );
}

function TaskTrackerResult({ result }: { result: Record<string, unknown> }) {
  const tasks = Array.isArray(result.tasks) ? (result.tasks as unknown[]) : [];
  const summary = (result.summary ?? {}) as Record<string, unknown>;
  const total =
    typeof summary.total === "number" ? summary.total : tasks.length;
  const done = typeof summary.done === "number" ? summary.done : 0;
  const inProgress =
    typeof summary.in_progress === "number" ? summary.in_progress : 0;
  const todo = typeof summary.todo === "number" ? summary.todo : 0;

  return (
    <div className="grid gap-1.5">
      <div className="flex items-center gap-2 text-[0.7rem] text-[var(--color-text-muted)]">
        <span>{total} total</span>
        {done ? (
          <span style={{ color: "var(--color-success)" }}>✓ {done} done</span>
        ) : null}
        {inProgress ? (
          <span style={{ color: "var(--color-accent)" }}>
            ◐ {inProgress} in progress
          </span>
        ) : null}
        {todo ? <span>○ {todo} todo</span> : null}
      </div>
      {tasks.length > 0 ? <TaskList tasks={tasks} /> : null}
    </div>
  );
}
