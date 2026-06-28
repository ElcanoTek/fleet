"use client";

// Tool-call rendering family extracted from chat-experience.tsx (slice 3 of
// #169). These components turn an agent ToolCall (and run_python output) into
// the collapsible chips and per-tool input/result views shown under an
// assistant turn. The whole family is self-contained: it depends only on the
// shared history.ts helpers/types, the byte formatters, and the syntax
// highlighter — never on the ChatExperience component's mutable state. Moving
// it here is a pure relocation; behavior, markup, and class names are
// unchanged.

import { useState } from "react";
// Syntax highlighter: Prism light build so we only ship the languages we
// actually render. PrismLight shares a single module-level grammar registry
// across imports, so registering the languages here (the only place that now
// renders a SyntaxHighlighter) makes them available wherever the highlighter
// is used. Registration was relocated verbatim from chat-experience.tsx when
// CodeBlock moved into this module (slice 3 of #169).
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
import {
  prettyToolName,
  safePretty,
  toolIcon,
  type Message,
  type PythonStream,
  type ToolCall,
} from "./history";

// ── Python output block ──────────────────────────────────────────────────
//
// Renders a run_python result as terminal-style output — stdout in the
// default monospace color, stderr tinted red. Empty output is suppressed
// so we don't render an empty black box. Execution time (when the bridge
// reports it) is shown as a small footer.

export function PythonOutput({ stream }: { stream: PythonStream }) {
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

// ── Tool chip ────────────────────────────────────────────────────────────

export function ToolChip({ tc, taskTrackerDisplay }: { tc: ToolCall; taskTrackerDisplay: TaskTrackerDisplay | null }) {
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

export function taskTrackerDisplayForMessage(message: Message): TaskTrackerDisplay | null {
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
