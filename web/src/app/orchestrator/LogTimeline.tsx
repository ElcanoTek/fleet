"use client";

// The Operations Center transcript, rendered as a timeline (see logTimeline.ts
// for the model). One card per tool call with its result folded in, colored by
// tool family; code and JSON syntax-highlighted; python and shell output shown
// as terminal output instead of their JSON envelopes; runtime breadcrumbs as
// slim event rows; the task prompt collapsed behind its first lines. The live
// view (LogViewer's LiveTaskView) reuses ToolStepCard so a running task and a
// finished one read the same.
//
// The syntax highlighter and the markdown pipeline are lazy chunks: a step's
// code is highlighted only when the step is opened, and until the chunk lands
// the same text shows unhighlighted, so nothing shifts.

import { lazy, memo, Suspense, useCallback, useMemo, useState, type ReactNode } from "react";
import type { LogMessage } from "@/app/shared/lib/orchestratorApi";
import { stripAnsiCodes } from "@/app/shared/lib/format";
import {
  buildTimeline,
  formatDuration,
  itemSearchText,
  languageForPath,
  parseJsonObject,
  parseToolResult,
  readableToolError,
  stepFailed,
  toolCallSummary,
  toolMeta,
  type ParsedResult,
  type RuntimeEvent,
  type TimelineItem,
  type ToolStep,
} from "./logTimeline";

const HighlightedCode = lazy(() => import("@/app/chat/ui/CodeHighlight"));
const LogMarkdown = lazy(() => import("./LogMarkdown"));

// Beyond this a highlighted block costs more than it helps (a 100 KB HTML
// body); it renders as plain text instead.
const HIGHLIGHT_MAX_CHARS = 60_000;

// ── code and output blocks ───────────────────────────────────────────────────

/**
 * Clamped shows the first `maxLines` lines of a long block with a control to
 * reveal the rest. Long python cells and protocol files are the common case:
 * the head is what a reader scans, the tail is one click away.
 */
function useClamp(text: string, maxLines: number) {
  const [all, setAll] = useState(false);
  const lines = useMemo(() => text.split("\n"), [text]);
  const clamped = !all && lines.length > maxLines;
  const shown = clamped ? lines.slice(0, maxLines).join("\n") : text;
  const toggle =
    lines.length > maxLines ? (
      <button type="button" className="lt-clamp-toggle" onClick={() => setAll((v) => !v)}>
        {all ? "Show less" : `Show all ${lines.length.toLocaleString()} lines`}
      </button>
    ) : null;
  return { shown, toggle };
}

export function CodeView({
  code,
  language,
  lineNumbers = false,
  wrap = false,
  maxLines = 40,
}: {
  code: string;
  language?: string;
  lineNumbers?: boolean;
  wrap?: boolean;
  maxLines?: number;
}) {
  const { shown, toggle } = useClamp(code, maxLines);
  const plain = (
    <pre className={`lt-code-plain${wrap ? " lt-code-plain--wrap" : ""}`}>{shown}</pre>
  );
  return (
    <div className="lt-code">
      <div className="lt-code-scroll">
        {language && shown.length <= HIGHLIGHT_MAX_CHARS ? (
          <Suspense fallback={plain}>
            <HighlightedCode
              code={shown}
              language={language}
              fontSize="0.78rem"
              showLineNumbers={lineNumbers}
              wrapLongLines={wrap}
            />
          </Suspense>
        ) : (
          plain
        )}
      </div>
      {toggle}
    </div>
  );
}

function TerminalOutput({ stdout, stderr, error }: { stdout: string; stderr: string; error?: string }) {
  const out = useClamp(stripAnsiCodes(stdout).replace(/\n+$/, ""), 40);
  const errText = stripAnsiCodes([stderr, error ?? ""].filter((s) => s.trim()).join("\n")).replace(/\n+$/, "");
  const err = useClamp(errText, 30);
  if (!out.shown && !errText) {
    return <div className="lt-terminal lt-terminal--empty">(no output)</div>;
  }
  return (
    <div className="lt-terminal">
      {out.shown ? <pre className="lt-terminal-out">{out.shown}</pre> : null}
      {out.toggle}
      {errText ? <pre className="lt-terminal-err">{err.shown}</pre> : null}
      {err.toggle}
    </div>
  );
}

function TextBlock({ text, maxLines = 30 }: { text: string; maxLines?: number }) {
  const { shown, toggle } = useClamp(text, maxLines);
  return (
    <div className="lt-text">
      <pre>{shown}</pre>
      {toggle}
    </div>
  );
}

function Section({ label, aside, children }: { label: string; aside?: ReactNode; children: ReactNode }) {
  return (
    <div className="lt-section">
      <div className="lt-section-head">
        <span className="lt-section-label">{label}</span>
        {aside ? <span className="lt-section-aside">{aside}</span> : null}
      </div>
      {children}
    </div>
  );
}

function Pill({ tone, children }: { tone: "ok" | "error" | "warn" | "neutral" | "running"; children: ReactNode }) {
  return <span className={`lt-pill lt-pill--${tone}`}>{children}</span>;
}

// ── tool input ───────────────────────────────────────────────────────────────

type PlanItem = { title: string; status: string; notes: string };

function planItems(list: unknown): PlanItem[] {
  if (!Array.isArray(list)) return [];
  return list.map((raw) => {
    const t = (raw ?? {}) as Record<string, unknown>;
    return {
      title: typeof t.title === "string" ? t.title : "(untitled)",
      status: typeof t.status === "string" ? t.status : "todo",
      notes: typeof t.notes === "string" ? t.notes : "",
    };
  });
}

function PlanList({ items }: { items: PlanItem[] }) {
  return (
    <ul className="lt-plan">
      {items.map((t, i) => (
        <li key={`${i}-${t.title}`} className={`lt-plan-item lt-plan-item--${t.status}`}>
          <span className="lt-plan-glyph" aria-hidden="true">
            {t.status === "done" ? "✓" : t.status === "in_progress" ? "◐" : "○"}
          </span>
          <span className="lt-plan-copy">
            <span className="lt-plan-title">{t.title}</span>
            {t.notes ? <span className="lt-plan-notes">{t.notes}</span> : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

function valueLanguage(key: string, value: string): string | undefined {
  if (/html/i.test(key) || /^\s*<(!doctype|html|div|table|p|body|tr)\b/i.test(value)) return "markup";
  if (key === "code" || key === "script") return "python";
  if (key === "command") return "bash";
  return undefined;
}

/**
 * ArgsList renders an argument object as labelled rows: short scalars inline,
 * long strings and nested values as their own (highlighted) blocks. Reads far
 * better than the JSON it came from, and keeps every value.
 */
function ArgsList({ args, skip = [] }: { args: Record<string, unknown>; skip?: string[] }) {
  const entries = Object.entries(args).filter(([k]) => !skip.includes(k));
  if (entries.length === 0) return null;
  return (
    <dl className="lt-args">
      {entries.map(([k, v]) => {
        let node: ReactNode;
        if (v === null || v === undefined) {
          node = <code className="lt-arg-null">null</code>;
        } else if (typeof v === "string") {
          node =
            v.length > 120 || v.includes("\n") ? (
              <CodeView code={v} language={valueLanguage(k, v)} wrap maxLines={16} />
            ) : (
              <code className="lt-arg-str">{v}</code>
            );
        } else if (typeof v === "number" || typeof v === "boolean") {
          node = <code className="lt-arg-num">{String(v)}</code>;
        } else if (Array.isArray(v) && v.every((x) => typeof x === "string" && x.length < 80) && v.length <= 12) {
          node = (
            <span className="lt-arg-list">
              {(v as string[]).map((x, i) => (
                <code key={`${i}-${x}`} className="lt-arg-str">
                  {x}
                </code>
              ))}
            </span>
          );
        } else {
          node = <CodeView code={JSON.stringify(v, null, 2)} language="json" maxLines={16} />;
        }
        return (
          <div key={k} className="lt-arg">
            <dt>{k}</dt>
            <dd>{node}</dd>
          </div>
        );
      })}
    </dl>
  );
}

function AuditInput({ args }: { args: Record<string, unknown> }) {
  const pass = args.success !== false;
  const summary = typeof args.user_visible_summary === "string" ? args.user_visible_summary : "";
  const reasoning = typeof args.reasoning === "string" ? args.reasoning : "";
  return (
    <div className="lt-audit">
      <div className="lt-audit-verdict">
        <Pill tone={pass ? "ok" : "error"}>{pass ? "Audit passes" : "Audit aborts the run"}</Pill>
        {summary ? <span className="lt-audit-summary">{summary}</span> : null}
      </div>
      {reasoning ? <p className="lt-audit-reasoning">{reasoning}</p> : null}
      <ArgsList args={args} skip={["success", "user_visible_summary", "reasoning"]} />
    </div>
  );
}

export function ToolInput({ name, argsRaw }: { name: string; argsRaw: string }) {
  const args = parseJsonObject(argsRaw);
  if (!args) {
    return argsRaw.trim() ? (
      <Section label="Input">
        <CodeView code={argsRaw} wrap maxLines={20} />
      </Section>
    ) : null;
  }
  if (name === "run_python" && typeof args.code === "string") {
    return (
      <Section label="Code" aside="python">
        <CodeView code={args.code} language="python" lineNumbers maxLines={60} />
      </Section>
    );
  }
  if (name === "bash" && typeof args.command === "string") {
    const cwd = typeof args.working_dir === "string" ? args.working_dir : "";
    return (
      <Section label="Command" aside={cwd ? `cwd ${cwd}` : "shell"}>
        <CodeView code={args.command} language="bash" wrap maxLines={30} />
      </Section>
    );
  }
  if (name === "task_tracker") {
    const items = planItems(args.task_list);
    return items.length > 0 ? (
      <Section label={`Plan · ${String(args.command ?? "update")}`}>
        <PlanList items={items} />
      </Section>
    ) : (
      <Section label="Input">
        <ArgsList args={args} />
      </Section>
    );
  }
  if (name === "confirm_audit") {
    return (
      <Section label="Audit">
        <AuditInput args={args} />
      </Section>
    );
  }
  if (name === "write_file" && typeof args.path === "string" && typeof args.content === "string") {
    return (
      <Section label="Write" aside={args.path}>
        <CodeView code={args.content} language={languageForPath(args.path)} lineNumbers maxLines={40} />
      </Section>
    );
  }
  if (name === "edit_file" && typeof args.path === "string") {
    const oldText = typeof args.old_text === "string" ? args.old_text : "";
    const newText = typeof args.new_text === "string" ? args.new_text : "";
    return (
      <Section label="Edit" aside={args.path}>
        <div className="lt-diff">
          {oldText ? <pre className="lt-diff-del">{oldText}</pre> : null}
          {newText ? <pre className="lt-diff-add">{newText}</pre> : null}
        </div>
      </Section>
    );
  }
  if (name === "spawn_subagent" && typeof args.task === "string") {
    return (
      <Section label="Delegated task" aside={typeof args.role === "string" ? args.role : "explore"}>
        <TextBlock text={args.task} maxLines={20} />
      </Section>
    );
  }
  return (
    <Section label="Arguments">
      <ArgsList args={args} />
    </Section>
  );
}

// ── tool output ──────────────────────────────────────────────────────────────

function Notes({ text }: { text: string }) {
  if (!text) return null;
  return <div className="lt-note">{text}</div>;
}

export function ToolOutput({
  name,
  argsRaw,
  resultRaw,
  isError,
  pending,
}: {
  name: string;
  argsRaw: string;
  resultRaw?: string;
  isError: boolean;
  pending?: boolean;
}) {
  if (resultRaw === undefined) {
    return pending ? <div className="lt-waiting">Waiting for the result…</div> : null;
  }
  const parsed: ParsedResult = parseToolResult(name, resultRaw, isError);
  if (parsed.kind === "python" || parsed.kind === "shell") {
    const aside = (
      <>
        {parsed.kind === "shell" ? (
          <Pill tone={parsed.exitCode === 0 ? "ok" : "error"}>exit {parsed.exitCode}</Pill>
        ) : (
          <Pill tone={parsed.ok ? "ok" : "error"}>{parsed.ok ? "ok" : "error"}</Pill>
        )}
        {parsed.ms !== undefined ? <span>{parsed.ms.toLocaleString()} ms</span> : null}
      </>
    );
    return (
      <Section label="Output" aside={aside}>
        <TerminalOutput
          stdout={parsed.stdout}
          stderr={parsed.stderr}
          error={parsed.kind === "python" ? parsed.error : undefined}
        />
        <Notes text={parsed.notes} />
      </Section>
    );
  }
  if (parsed.kind === "json") {
    const obj = parseJsonObject(parsed.pretty);
    if (name === "task_tracker" && obj && Array.isArray(obj.tasks)) {
      return (
        <Section label="Plan" aside={typeof obj.active_task === "string" && obj.active_task ? `now: ${obj.active_task}` : undefined}>
          <PlanList items={planItems(obj.tasks)} />
        </Section>
      );
    }
    return (
      <Section
        label="Result"
        aside={parsed.headline ? <Pill tone={parsed.ok ? "neutral" : "error"}>{parsed.headline}</Pill> : undefined}
      >
        <CodeView code={parsed.pretty} language="json" maxLines={30} />
        <Notes text={parsed.notes} />
      </Section>
    );
  }
  // Plain text: a file the agent viewed gets its language back from the path.
  const args = parseJsonObject(argsRaw);
  const path = args && typeof args.path === "string" ? args.path : "";
  const lang = name === "view_file" && path ? languageForPath(path) : undefined;
  return (
    <Section label={parsed.ok ? "Result" : "Error"} aside={parsed.headline || undefined}>
      {lang ? <CodeView code={parsed.text} language={lang} lineNumbers maxLines={30} /> : <TextBlock text={parsed.text} />}
      <Notes text={parsed.notes} />
    </Section>
  );
}

// ── the step card ────────────────────────────────────────────────────────────

function Chevron() {
  return (
    <svg className="lt-chevron" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      <path d="M6 3.5 10.5 8 6 12.5" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function firstErrorLine(tool: ToolStep): string {
  if (tool.resultRaw === undefined) return "";
  const parsed = parseToolResult(tool.name, tool.resultRaw, tool.isError);
  if (parsed.ok) return "";
  let text = "";
  if (parsed.kind === "python") text = parsed.error || parsed.stderr || parsed.stdout;
  else if (parsed.kind === "shell") text = parsed.stderr || parsed.stdout;
  else if (parsed.kind === "json") text = parsed.headline;
  else text = parsed.text;
  const lines = stripAnsiCodes(text).split("\n").map((l) => l.trim()).filter(Boolean);
  // A traceback's useful line is its last one ("KeyError: 'spend'").
  const line = parsed.kind === "python" && lines.length > 1 ? lines[lines.length - 1] : lines[0] ?? "";
  return readableToolError(line).slice(0, 300);
}

export const ToolStepCard = memo(function ToolStepCard({
  tool,
  open,
  onToggle,
  testId,
}: {
  tool: ToolStep;
  open: boolean;
  onToggle: (key: string) => void;
  testId?: string;
}) {
  const meta = toolMeta(tool.name);
  const args = parseJsonObject(tool.argsRaw);
  const summary = toolCallSummary(tool.name, args);
  const failed = stepFailed(tool);
  const errorLine = failed ? firstErrorLine(tool) : "";
  // Log timestamps are whole seconds, so only a call that took at least one
  // shows a duration; "<1s" on every row was noise.
  const took =
    tool.startedAt !== undefined && tool.endedAt !== undefined && tool.endedAt - tool.startedAt >= 1
      ? formatDuration(tool.endedAt - tool.startedAt)
      : "";
  const status = tool.pending ? (
    <Pill tone="running">Running…</Pill>
  ) : failed ? (
    <Pill tone="error">
      Failed{(tool.repeats ?? 1) > 1 ? ` · ${tool.repeats} attempts` : ""}
    </Pill>
  ) : tool.resultRaw !== undefined ? (
    <Pill tone="ok">Done</Pill>
  ) : null;
  return (
    <article
      id={`lt-${tool.key}`}
      className={`lt-step lt-cat--${meta.category}${failed ? " lt-step--error" : ""}${open ? " lt-step--open" : ""}`}
      data-testid={testId ?? "log-tool-step"}
    >
      <button type="button" className="lt-step-head" aria-expanded={open} onClick={() => onToggle(tool.key)}>
        <span className="lt-step-num">{tool.step}</span>
        <span className="lt-step-icon" aria-hidden="true">
          {meta.icon}
        </span>
        <span className="lt-step-name" title={tool.name}>
          {meta.server ? <span className="lt-step-server">{meta.server}</span> : null}
          <span className="lt-step-action">{meta.action}</span>
        </span>
        <span className="lt-step-summary">{summary}</span>
        <span className="lt-step-meta">
          {took ? <span className="lt-step-took">{took}</span> : null}
          {status}
          <Chevron />
        </span>
      </button>
      {errorLine && !open ? <p className="lt-step-error">{errorLine}</p> : null}
      {open ? (
        <div className="lt-step-body">
          {/* task_tracker echoes the plan in its result (the authoritative
              state), so showing the input too renders the list twice. */}
          {tool.name === "task_tracker" && tool.resultRaw ? null : (
            <ToolInput name={tool.name} argsRaw={tool.argsRaw} />
          )}
          <ToolOutput
            name={tool.name}
            argsRaw={tool.argsRaw}
            resultRaw={tool.resultRaw}
            isError={tool.isError}
            pending={tool.pending}
          />
        </div>
      ) : null}
    </article>
  );
});

// ── non-tool items ───────────────────────────────────────────────────────────

function clock(ts?: number): string {
  return ts ? new Date(ts * 1000).toLocaleTimeString() : "";
}

function Markdown({ content, taskId }: { content: string; taskId: string }) {
  return (
    <div className="lt-md">
      <Suspense fallback={<div className="whitespace-pre-wrap">{content}</div>}>
        <LogMarkdown content={content} taskId={taskId} />
      </Suspense>
    </div>
  );
}

function PromptCard({ msg }: { msg: LogMessage }) {
  const [open, setOpen] = useState(false);
  const content = msg.content ?? "";
  const long = content.split("\n").length > 14 || content.length > 1600;
  return (
    <section className="lt-prompt" data-testid="log-task-prompt">
      <header className="lt-prompt-head">
        <span className="lt-prompt-title">Task prompt</span>
        <span className="lt-prompt-meta">
          {content.length.toLocaleString()} characters
          {msg.created_at ? ` · ${clock(msg.created_at)}` : ""}
        </span>
        {long ? (
          <button
            type="button"
            className="btn btn-secondary btn-small"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
          >
            {open ? "Collapse" : "Show full prompt"}
          </button>
        ) : null}
      </header>
      <div className={`lt-prompt-body${long && !open ? " lt-prompt-body--clamped" : ""}`}>
        <pre>{content}</pre>
      </div>
    </section>
  );
}

function AssistantCard({ msg, final, taskId }: { msg: LogMessage; final: boolean; taskId: string }) {
  const content = stripAnsiCodes(msg.content ?? "");
  const reasoning = (msg.reasoning ?? "").trim();
  return (
    <section className={`lt-assistant${final ? " lt-assistant--final" : ""}`} data-testid="log-assistant">
      <header className="lt-assistant-head">
        <span className="lt-assistant-title">{final ? "Final answer" : "Assistant"}</span>
        {msg.model ? <span className="lt-assistant-model">{msg.model}</span> : null}
        <span className="lt-assistant-time">{clock(msg.created_at)}</span>
      </header>
      {reasoning ? (
        <details className="lt-reasoning">
          <summary>Reasoning</summary>
          <pre>{stripAnsiCodes(reasoning)}</pre>
        </details>
      ) : null}
      {content.trim() ? (
        <div className="lt-assistant-body">
          <Markdown content={content} taskId={taskId} />
        </div>
      ) : null}
    </section>
  );
}

const EVENT_FIELD_LABEL: Record<string, (v: string) => string> = {
  removed_turns: (v) => `${v} turns summarized`,
  used: (v) => `${Number(v).toLocaleString()} tokens resent`,
  trigger: (v) => v.replace(/_/g, " "),
  effective_budget: (v) => `budget ${Number(v).toLocaleString()}`,
};

function eventFacts(event: RuntimeEvent): string {
  const known = event.fields.filter(([k]) => EVENT_FIELD_LABEL[k]).map(([k, v]) => EVENT_FIELD_LABEL[k](v));
  if (known.length > 0) return known.join(" · ");
  return event.fields
    .slice(0, 3)
    .map(([k, v]) => `${k} ${v}`)
    .join(" · ");
}

function EventRow({ event, msg }: { event: RuntimeEvent; msg: LogMessage }) {
  const [open, setOpen] = useState(event.tone === "danger");
  const facts = eventFacts(event);
  if (event.tone === "danger") {
    return (
      <div className="lt-event-banner" role="alert" data-testid="log-event">
        <span className="lt-event-banner-title">{event.title}</span>
        <p>{event.detail || msg.content}</p>
      </div>
    );
  }
  return (
    <div className={`lt-event lt-event--${event.tone}`} data-testid="log-event">
      <button type="button" className="lt-event-pill" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <span className="lt-event-title">{event.title}</span>
        {facts ? <span className="lt-event-facts">{facts}</span> : null}
        {msg.created_at ? <span className="lt-event-time">{clock(msg.created_at)}</span> : null}
      </button>
      {open ? <p className="lt-event-detail">{msg.content}</p> : null}
    </div>
  );
}

function SummaryCard({ msg, taskId }: { msg: LogMessage; taskId: string }) {
  return (
    <details className="lt-summary" data-testid="log-compaction-summary">
      <summary>
        <span className="lt-summary-title">Compaction summary</span>
        <span className="lt-summary-hint">what the run continued from after the history was compacted</span>
      </summary>
      <div className="lt-summary-body">
        <Markdown content={(msg.content ?? "").replace(/^\[context compaction\]\s*/, "")} taskId={taskId} />
      </div>
    </details>
  );
}

function UserCard({ msg, taskId }: { msg: LogMessage; taskId: string }) {
  return (
    <section className="lt-user" data-testid="log-user">
      <header className="lt-user-head">
        <span>{msg.role === "user" ? "Message to the agent" : msg.role || "Message"}</span>
        <span className="lt-assistant-time">{clock(msg.created_at)}</span>
      </header>
      <div className="lt-user-body">
        <Markdown content={msg.content ?? ""} taskId={taskId} />
      </div>
    </section>
  );
}

// ── overview strip ───────────────────────────────────────────────────────────

/**
 * StepStrip is the whole run at a glance: one tick per tool call, colored by
 * tool family, failures ringed, compactions as gaps. Clicking a tick opens and
 * scrolls to that step — the quick way through a 130-step transcript.
 */
function StepStrip({ items, onJump }: { items: TimelineItem[]; onJump: (key: string) => void }) {
  const ticks = items.filter((it) => it.kind === "tool" || (it.kind === "event" && it.event.tag === "context_compacted"));
  if (ticks.filter((t) => t.kind === "tool").length < 2) return null;
  return (
    <div className="lt-strip" data-testid="log-step-strip" aria-label="Run overview">
      {ticks.map((it) => {
        if (it.kind !== "tool") return <span key={it.key} className="lt-strip-gap" title="History compacted" />;
        const meta = toolMeta(it.tool.name);
        const failed = stepFailed(it.tool);
        return (
          <button
            key={it.key}
            type="button"
            className={`lt-strip-tick lt-cat--${meta.category}${failed ? " lt-strip-tick--error" : ""}`}
            title={`#${it.tool.step} ${it.tool.name}${failed ? " — failed" : ""}`}
            aria-label={`Step ${it.tool.step}: ${it.tool.name}${failed ? ", failed" : ""}`}
            onClick={() => onJump(it.key)}
          />
        );
      })}
    </div>
  );
}

// ── filters ──────────────────────────────────────────────────────────────────

type FilterChip = { key: string; label: string; count: number; category?: string };
type FilterGroups = Array<{ title: string; chips: FilterChip[] }>;

/**
 * buildFilterIndex tags every timeline item with the chips it matches.
 * Selecting chips narrows the timeline to items with ANY selected tag (union);
 * no selection shows everything. Tags are per item, not per message: one
 * assistant message can carry several calls, and a failed call must not drag
 * its healthy siblings into the Errors view.
 */
export function buildFilterIndex(items: TimelineItem[]): { tags: Array<Set<string>>; groups: FilterGroups } {
  const tags = items.map((it) => {
    const t = new Set<string>();
    if (it.kind === "tool") {
      t.add("hl:tool-calls");
      t.add(`tool:${it.tool.name}`);
      if (it.tool.resultRaw !== undefined) t.add("hl:tool-results");
      if (it.tool.name === "task_tracker") t.add("hl:task-tracker");
      if (stepFailed(it.tool)) t.add("hl:errors");
      return t;
    }
    const m = it.msg;
    if (it.kind === "assistant") {
      if ((m.content ?? "").trim()) t.add("hl:responses");
      if ((m.reasoning ?? "").trim()) t.add("hl:reasoning");
    }
    if (it.kind === "event" || it.kind === "summary") t.add("hl:events");
    if (it.kind === "event" && it.event.tone === "danger") t.add("hl:errors");
    if (m.model) t.add(`model:${m.model}`);
    if (m.provider) t.add(`provider:${m.provider}`);
    return t;
  });
  const count = (key: string) => tags.filter((t) => t.has(key)).length;
  const collect = (prefix: string, category?: (label: string) => string) => {
    const keys = new Set<string>();
    for (const t of tags) for (const k of t) if (k.startsWith(prefix)) keys.add(k);
    return [...keys].sort().map((k) => {
      const label = k.slice(prefix.length);
      return { key: k, label, count: count(k), category: category?.(label) };
    });
  };
  const highlights: FilterChip[] = [
    { key: "hl:errors", label: "Errors", count: count("hl:errors") },
    { key: "hl:reasoning", label: "Reasoning", count: count("hl:reasoning") },
    { key: "hl:responses", label: "Responses", count: count("hl:responses") },
    { key: "hl:tool-calls", label: "Tool calls", count: count("hl:tool-calls") },
    { key: "hl:tool-results", label: "Tool results", count: count("hl:tool-results") },
    { key: "hl:task-tracker", label: "Task tracker", count: count("hl:task-tracker") },
    { key: "hl:events", label: "Runtime events", count: count("hl:events") },
  ].filter((c) => c.count > 0);
  const groups: FilterGroups = [
    { title: "Highlights", chips: highlights },
    { title: "Tools", chips: collect("tool:", (label) => toolMeta(label).category) },
    { title: "Models", chips: collect("model:") },
    { title: "Providers", chips: collect("provider:") },
  ].filter((g) => g.chips.length > 0);
  return { tags, groups };
}

function Filters({
  groups,
  selected,
  onToggle,
  onClear,
  shown,
  total,
  query,
  onQuery,
  onExpandAll,
  onCollapseAll,
}: {
  groups: FilterGroups;
  selected: Set<string>;
  onToggle: (key: string) => void;
  onClear: () => void;
  shown: number;
  total: number;
  query: string;
  onQuery: (q: string) => void;
  onExpandAll: () => void;
  onCollapseAll: () => void;
}) {
  return (
    <div className="log-filters" data-testid="log-filters">
      <div className="log-filters-head">
        <input
          type="search"
          className="lt-search"
          placeholder="Search this run — tool, file, error text…"
          aria-label="Search the transcript"
          value={query}
          onChange={(e) => onQuery(e.target.value)}
        />
        <span className="lt-filter-actions">
          <button type="button" className="btn btn-secondary btn-small" onClick={onExpandAll}>
            Expand all
          </button>
          <button type="button" className="btn btn-secondary btn-small" onClick={onCollapseAll}>
            Collapse all
          </button>
          {selected.size > 0 || query ? (
            <button type="button" className="btn btn-secondary btn-small" onClick={onClear}>
              Clear all
            </button>
          ) : null}
        </span>
      </div>
      {groups.map((g) => (
        <div key={g.title} className="log-filter-group">
          <span className="log-filter-group-title">{g.title}</span>
          <div className="log-filter-chips">
            {g.chips.map((c) => (
              <button
                key={c.key}
                type="button"
                className={`log-chip${selected.has(c.key) ? " log-chip--active" : ""}${c.key === "hl:errors" ? " log-chip--errors" : ""}${c.category ? ` lt-cat--${c.category}` : ""}`}
                aria-pressed={selected.has(c.key)}
                onClick={() => onToggle(c.key)}
              >
                {c.category ? <span className="log-chip-dot" aria-hidden="true" /> : null}
                {c.label} <span className="log-chip-count">{c.count}</span>
              </button>
            ))}
          </div>
        </div>
      ))}
      <div className="log-filters-shown">
        {selected.size === 0 && !query ? `Showing all ${total} messages` : `Showing ${shown} of ${total} messages`}
      </div>
    </div>
  );
}

// ── the transcript ───────────────────────────────────────────────────────────

/**
 * TranscriptTimeline renders a stored session: filters and search, the run
 * overview strip, then the timeline. `renderSubagent` lets the caller keep
 * owning the sub-agent child card (it fetches the child transcript).
 */
export function TranscriptTimeline({
  messages,
  taskId,
  renderSubagent,
  withFilters = true,
}: {
  messages: LogMessage[];
  taskId: string;
  renderSubagent?: (msg: LogMessage, key: string) => ReactNode;
  withFilters?: boolean;
}) {
  const items = useMemo(() => buildTimeline(messages), [messages]);
  const { tags, groups } = useMemo(() => buildFilterIndex(items), [items]);
  const searchText = useMemo(() => items.map(itemSearchText), [items]);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [query, setQuery] = useState("");
  const [open, setOpen] = useState<Set<string>>(new Set());

  const q = query.trim().toLowerCase();
  const visible = useMemo(
    () =>
      items.filter((it, idx) => {
        if (selected.size > 0 && ![...selected].some((k) => tags[idx].has(k))) return false;
        if (q && !searchText[idx].includes(q)) return false;
        return true;
      }),
    [items, selected, tags, q, searchText],
  );
  const shownMessages = useMemo(() => new Set(visible.flatMap((it) => it.members)).size, [visible]);

  const toggleOpen = useCallback(
    (key: string) =>
      setOpen((prev) => {
        const next = new Set(prev);
        if (next.has(key)) next.delete(key);
        else next.add(key);
        return next;
      }),
    [],
  );
  const jump = useCallback((key: string) => {
    setOpen((prev) => new Set(prev).add(key));
    // After the opened card renders.
    window.requestAnimationFrame(() =>
      document.getElementById(`lt-${key}`)?.scrollIntoView?.({ block: "start", behavior: "smooth" }),
    );
  }, []);

  return (
    <>
      {withFilters ? (
        <Filters
          groups={groups}
          selected={selected}
          onToggle={(key) =>
            setSelected((prev) => {
              const next = new Set(prev);
              if (next.has(key)) next.delete(key);
              else next.add(key);
              return next;
            })
          }
          onClear={() => {
            setSelected(new Set());
            setQuery("");
          }}
          shown={shownMessages}
          total={messages.length}
          query={query}
          onQuery={setQuery}
          onExpandAll={() => setOpen(new Set(visible.filter((it) => it.kind === "tool").map((it) => it.key)))}
          onCollapseAll={() => setOpen(new Set())}
        />
      ) : null}
      {withFilters ? <StepStrip items={items} onJump={jump} /> : null}
      <div className="log-session lt-timeline">
        {visible.length === 0 ? <div className="table-empty">Nothing in this run matches.</div> : null}
        {visible.map((it) => {
          switch (it.kind) {
            case "prompt":
              return <PromptCard key={it.key} msg={it.msg} />;
            case "assistant":
              return <AssistantCard key={it.key} msg={it.msg} final={it.final} taskId={taskId} />;
            case "tool":
              return <ToolStepCard key={it.key} tool={it.tool} open={open.has(it.key)} onToggle={toggleOpen} />;
            case "event":
              return <EventRow key={it.key} event={it.event} msg={it.msg} />;
            case "summary":
              return <SummaryCard key={it.key} msg={it.msg} taskId={taskId} />;
            case "subagent":
              return <div key={it.key}>{renderSubagent?.(it.msg, it.key) ?? <UserCard msg={it.msg} taskId={taskId} />}</div>;
            default:
              return <UserCard key={it.key} msg={it.msg} taskId={taskId} />;
          }
        })}
      </div>
    </>
  );
}
