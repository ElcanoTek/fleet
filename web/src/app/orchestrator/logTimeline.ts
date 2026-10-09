// Pure model behind the task-log timeline (LogTimeline.tsx). A stored
// transcript is a flat list of messages — the task prompt, assistant turns,
// one tool-role message per result, and runtime breadcrumbs the engine writes
// as user-role "[tag] …" lines. Rendered one card per message it read as a
// wall of raw JSON: every tool call split across two cards, run_python code
// shown as an escaped one-line JSON string, the stdout printed twice inside
// its envelope. This module turns that list into steps a person can scan:
// each call paired with its result, results parsed per tool, breadcrumbs
// recognized as events. No React here, so all of it is unit-tested directly.

import type { LogMessage, LogToolCall } from "@/app/shared/lib/orchestratorApi";

// ── tool identity ────────────────────────────────────────────────────────────

export type ToolCategory =
  | "python"
  | "shell"
  | "files"
  | "plan"
  | "audit"
  | "email"
  | "send"
  | "web"
  | "pages"
  | "delegate"
  | "mcp"
  | "other";

export type ToolMeta = {
  category: ToolCategory;
  /** Short category label shown on chips ("Python", "Email", …). */
  categoryLabel: string;
  /** MCP server the tool belongs to, when the name says so. */
  server?: string;
  /** The tool's own name without the mcp_<server>_ prefix. */
  action: string;
  icon: string;
};

const CATEGORY_LABEL: Record<ToolCategory, string> = {
  python: "Python",
  shell: "Shell",
  files: "Files",
  plan: "Plan",
  audit: "Audit",
  email: "Email",
  send: "Outbound",
  web: "Web",
  pages: "Pages",
  delegate: "Delegation",
  mcp: "MCP",
  other: "Tool",
};

const CATEGORY_ICON: Record<ToolCategory, string> = {
  python: "🐍",
  shell: "❯",
  files: "📄",
  plan: "☑",
  audit: "🛡",
  email: "✉",
  send: "📤",
  web: "🌐",
  pages: "📊",
  delegate: "🤖",
  mcp: "🔌",
  other: "🛠",
};

const BUILTIN_CATEGORY: Record<string, ToolCategory> = {
  run_python: "python",
  bash: "shell",
  view_file: "files",
  write_file: "files",
  edit_file: "files",
  list_files: "files",
  publish_artifact: "files",
  download_url: "files",
  xlsx_workbook: "files",
  task_tracker: "plan",
  confirm_audit: "audit",
  web_fetch: "web",
  web_search: "web",
  smart_search: "web",
  spawn_subagent: "delegate",
  create_task: "delegate",
  send_email: "send",
};

// MCP server names that contain an underscore. Everything else splits at the
// first underscore after "mcp_" (mcp_email_search_emails → email /
// search_emails), which is the bundle convention for single-word servers.
const MULTIWORD_SERVERS = [
  "ses_outbound",
  "fast_io",
  "s3_feeds",
  "deal_sheet",
  "medianet_select",
  "google_drive",
  "google_sheets",
];

export function toolMeta(name: string): ToolMeta {
  const builtin = BUILTIN_CATEGORY[name];
  if (builtin) {
    return {
      category: builtin,
      categoryLabel: CATEGORY_LABEL[builtin],
      action: name,
      icon: CATEGORY_ICON[builtin],
    };
  }
  if (name.startsWith("mcp_")) {
    const rest = name.slice(4);
    const multi = MULTIWORD_SERVERS.find((s) => rest.startsWith(`${s}_`));
    const server = multi ?? rest.split("_")[0];
    const action = rest.slice(server.length + 1) || rest;
    let category: ToolCategory = "mcp";
    if (/send_email$|^send_/.test(action) || server === "ses_outbound" || server === "sendgrid") {
      category = "send";
    } else if (server === "email") {
      category = "email";
    } else if (server === "pages") {
      category = "pages";
    }
    return {
      category,
      categoryLabel: category === "mcp" ? server : CATEGORY_LABEL[category],
      server,
      action,
      icon: CATEGORY_ICON[category],
    };
  }
  return { category: "other", categoryLabel: CATEGORY_LABEL.other, action: name, icon: CATEGORY_ICON.other };
}

// ── deferred-call envelope ───────────────────────────────────────────────────

/**
 * unwrapDeferredCall resolves the `tool_call` bridge (a deferred tool invoked
 * through the generic envelope) to the real tool name and its arguments.
 * Calls emitted from the old schema wrapped the argument object in a
 * singleton array; those are shown as the server repairs them.
 */
export function unwrapDeferredCall(name: string | undefined, input: string | undefined): { name: string; input: string } {
  const fallback = { name: name || "tool", input: input ?? "" };
  if (name !== "tool_call" || !input) return fallback;
  try {
    const envelope = JSON.parse(input) as { name?: unknown; arguments?: unknown };
    if (typeof envelope.name !== "string" || !envelope.name) return fallback;
    let args = envelope.arguments;
    if (Array.isArray(args) && args.length === 1 && args[0] && typeof args[0] === "object") {
      args = args[0];
    }
    return { name: envelope.name, input: JSON.stringify(args ?? {}) };
  } catch {
    return fallback;
  }
}

export function readableToolError(raw: string): string {
  if (/cannot unmarshal array into Go value of type map\[string\]interface/i.test(raw)) {
    return "Invalid tool arguments: Fleet expected a JSON object but received an array.";
  }
  return raw;
}

// ── JSON helpers ─────────────────────────────────────────────────────────────

export function parseJsonObject(raw: string | undefined): Record<string, unknown> | null {
  if (!raw) return null;
  const trimmed = raw.trim();
  if (!trimmed.startsWith("{")) return null;
  try {
    const v = JSON.parse(trimmed) as unknown;
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

/**
 * splitFleetNotes separates the runtime's own annotations from a tool's
 * output. fleet appends lines such as "[fleet date-window] runtime_today=…"
 * after a JSON body, which makes the whole result unparseable; split them off
 * so the body parses and the note renders as a note.
 */
export function splitFleetNotes(raw: string): { body: string; notes: string } {
  const m = /\n+\[fleet [^\]\n]*\]/.exec(raw);
  if (!m) return { body: raw, notes: "" };
  return { body: raw.slice(0, m.index), notes: raw.slice(m.index).trim() };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function truncate(s: string, max: number): string {
  const one = s.replace(/\s+/g, " ").trim();
  return one.length > max ? `${one.slice(0, max - 1)}…` : one;
}

function scalarText(v: unknown): string | null {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return null;
}

// ── one-line call summaries ──────────────────────────────────────────────────

/**
 * toolCallSummary is the single line a collapsed step shows next to the tool
 * name: what the call was about, not its whole argument blob.
 */
export function toolCallSummary(name: string, args: Record<string, unknown> | null): string {
  if (!args) return "";
  if (name === "run_python") {
    const code = str(args.code);
    const lines = code.split("\n").map((l) => l.trim()).filter(Boolean);
    const first = lines[0] ?? "";
    const head = first.startsWith("#") ? first.replace(/^#+\s*/, "") : first;
    return truncate(head, 140) + (lines.length > 1 ? `  ·  ${lines.length} lines` : "");
  }
  if (name === "bash") return truncate(`$ ${str(args.command).split("\n")[0]}`, 140);
  if (name === "task_tracker") {
    const cmd = str(args.command) || "update";
    const list = Array.isArray(args.task_list) ? (args.task_list as Array<Record<string, unknown>>) : [];
    if (list.length === 0) return cmd;
    const done = list.filter((t) => t && t.status === "done").length;
    return `${cmd} · ${list.length} items · ${done} done`;
  }
  if (name === "confirm_audit") {
    const verdict = args.success === false ? "abort" : "pass";
    const why = str(args.user_visible_summary) || str(args.reasoning);
    return truncate(why ? `${verdict} — ${why}` : verdict, 160);
  }
  if (typeof args.path === "string" && args.path) {
    const extra = str(args.description);
    return truncate(extra ? `${args.path} — ${extra}` : args.path, 140);
  }
  if (typeof args.url === "string" && args.url) return truncate(args.url, 140);
  if (typeof args.query === "string" && args.query) return truncate(`“${args.query}”`, 140);

  const bits: string[] = [];
  const subject = str(args.subject_contains) || str(args.subject);
  if (subject) bits.push(`“${subject}”`);
  const sender = str(args.sender_contains);
  if (sender) bits.push(`from ${sender}`);
  const from = str(args.date_from);
  const to = str(args.date_to);
  if (from || to) bits.push(from && to && from !== to ? `${from} → ${to}` : from || to);
  const onOrBefore = str(args.on_or_before);
  if (onOrBefore) bits.push(`≤ ${onOrBefore}`);
  const filename = str(args.filename);
  if (filename) bits.push(filename);
  const toAddr = scalarText(args.to) ?? (Array.isArray(args.to) ? (args.to as unknown[]).join(", ") : "");
  if (toAddr) bits.push(`to ${toAddr}`);
  if (bits.length > 0) return truncate(bits.join(" · "), 160);

  for (const [k, v] of Object.entries(args)) {
    const s = scalarText(v);
    if (s !== null && s !== "") bits.push(`${k}=${s}`);
    if (bits.length === 3) break;
  }
  return truncate(bits.join(" · "), 160);
}

// ── results ──────────────────────────────────────────────────────────────────

export type ParsedResult =
  | { kind: "python"; ok: boolean; stdout: string; stderr: string; error: string; ms?: number; notes: string }
  | { kind: "shell"; ok: boolean; exitCode: number; stdout: string; stderr: string; ms?: number; notes: string }
  | { kind: "json"; ok: boolean; pretty: string; headline: string; notes: string }
  | { kind: "text"; ok: boolean; text: string; headline: string; notes: string };

const FAILED_STATUS = new Set(["error", "failed", "failure"]);

/**
 * parseToolResult reads one tool result into the shape its card renders:
 * python/shell envelopes become terminal output, other JSON is re-indented,
 * everything else stays text. `ok` folds every failure signal the runtime
 * uses — the explicit error bit, a "[tool error]" prefix, a status:"error"
 * body, a non-zero exit — into one flag.
 */
export function parseToolResult(name: string, content: string, isError: boolean): ParsedResult {
  const { body, notes } = splitFleetNotes(content);
  const errPrefix = /^\s*\[tool error\]/.test(body);
  const obj = parseJsonObject(body);

  if (name === "run_python" && obj && ("stdout" in obj || "output" in obj || "error" in obj)) {
    const error = str(obj.error);
    const status = str(obj.status);
    return {
      kind: "python",
      ok: !isError && !error && !FAILED_STATUS.has(status),
      stdout: str(obj.stdout) || (str(obj.stderr) || error ? "" : str(obj.output)),
      stderr: str(obj.stderr),
      error,
      ms: typeof obj.execution_time_ms === "number" ? obj.execution_time_ms : undefined,
      notes,
    };
  }
  if (name === "bash" && obj && ("exit_code" in obj || "stdout" in obj)) {
    const exitCode = typeof obj.exit_code === "number" ? obj.exit_code : 0;
    return {
      kind: "shell",
      ok: !isError && exitCode === 0 && !str(obj.error),
      exitCode,
      stdout: str(obj.stdout),
      stderr: str(obj.stderr) || str(obj.error),
      ms: typeof obj.execution_time_ms === "number" ? obj.execution_time_ms : undefined,
      notes,
    };
  }
  if (obj) {
    const status = str(obj.status);
    const failed = isError || errPrefix || FAILED_STATUS.has(status) || obj.valid === false || (typeof obj.error === "string" && obj.error !== "");
    return { kind: "json", ok: !failed, pretty: JSON.stringify(obj, null, 2), headline: jsonHeadline(obj), notes };
  }
  const trimmed = body.trim();
  const auditFailed = name === "confirm_audit" && /^Audit Failed/i.test(trimmed);
  const firstLine = trimmed.split("\n")[0] ?? "";
  return {
    kind: "text",
    ok: !isError && !errPrefix && !auditFailed,
    text: isError ? readableToolError(trimmed) : trimmed,
    headline: name === "confirm_audit" || name === "publish_artifact" ? truncate(firstLine, 140) : "",
    notes,
  };
}

function jsonHeadline(obj: Record<string, unknown>): string {
  const bits: string[] = [];
  if (typeof obj.valid === "boolean") {
    const errors = Array.isArray(obj.errors) ? obj.errors.length : 0;
    const warnings = Array.isArray(obj.warnings) ? obj.warnings.length : 0;
    bits.push(obj.valid ? "valid" : "invalid");
    if (errors) bits.push(`${errors} error${errors === 1 ? "" : "s"}`);
    if (warnings) bits.push(`${warnings} warning${warnings === 1 ? "" : "s"}`);
  }
  if (typeof obj.matches_found === "number") bits.push(`${obj.matches_found} match${obj.matches_found === 1 ? "" : "es"}`);
  else if (typeof obj.match_count === "number") bits.push(`${obj.match_count} match${obj.match_count === 1 ? "" : "es"}`);
  if (typeof obj.emails_scanned === "number") bits.push(`${obj.emails_scanned} scanned`);
  if (typeof obj.saved_to === "string") bits.push(`saved ${obj.saved_to.split("/").pop()}`);
  if (typeof obj.size_bytes === "number" && bits.length > 0) bits.push(formatBytes(obj.size_bytes));
  if (typeof obj.message_id === "string" && bits.length === 0) bits.push(`sent · ${truncate(obj.message_id, 40)}`);
  if (bits.length === 0 && typeof obj.status === "string") bits.push(obj.status);
  const err = str(obj.error) || str(obj.message);
  if (err && (obj.status === "error" || obj.error)) bits.push(truncate(err, 100));
  return bits.join(" · ");
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "";
  if (seconds < 1) return "<1s";
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return s ? `${m}m ${s}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Language for a file path, by extension, among the grammars the highlighter registers. */
export function languageForPath(path: string): string | undefined {
  const ext = path.toLowerCase().split(".").pop() ?? "";
  if (ext === "py") return "python";
  if (ext === "sh" || ext === "bash") return "bash";
  if (ext === "json") return "json";
  if (ext === "yaml" || ext === "yml") return "yaml";
  if (ext === "html" || ext === "htm" || ext === "xml" || ext === "svg") return "markup";
  return undefined;
}

// ── runtime events ───────────────────────────────────────────────────────────

export type RuntimeEvent = {
  tag: string;
  title: string;
  tone: "info" | "warn" | "danger";
  /** key=value pairs the engine wrote into the breadcrumb, in order. */
  fields: Array<[string, string]>;
  /** The human sentence after the em dash, or the whole remainder. */
  detail: string;
};

const EVENT_TITLES: Record<string, { title: string; tone: RuntimeEvent["tone"] }> = {
  context_checkpoint_floor: { title: "Context budget floor set", tone: "info" },
  context_checkpoint: { title: "Context checkpoint", tone: "info" },
  context_compacted: { title: "History compacted", tone: "info" },
  context_pressure: { title: "Context pressure", tone: "warn" },
  fatal: { title: "Run aborted", tone: "danger" },
  "plan state after compaction": { title: "Plan re-announced", tone: "info" },
  "context compaction": { title: "Compaction summary", tone: "info" },
};

// A bracketed tag at the very start, not followed by "(" — a message that
// opens with a markdown link is ordinary text.
const EVENT_TAG = /^\[([a-z][a-z0-9_ .:-]{1,60})\](?!\()\s*/i;

/**
 * parseRuntimeEvent recognizes the bracket-tagged breadcrumbs the engine
 * writes into the session log ("[context_compacted] trigger=… — why").
 * Returns null for anything else, so ordinary user text is never mistaken
 * for one.
 */
export function parseRuntimeEvent(content: string): RuntimeEvent | null {
  const m = EVENT_TAG.exec(content);
  if (!m) return null;
  const tag = m[1];
  const rest = content.slice(m[0].length);
  const dash = rest.indexOf(" — ");
  const head = dash >= 0 ? rest.slice(0, dash) : rest;
  const tail = dash >= 0 ? rest.slice(dash + 3) : "";
  const fields: Array<[string, string]> = [];
  for (const fm of head.matchAll(/([a-z_]+)=([^\s]+)/gi)) fields.push([fm[1], fm[2]]);
  const known = EVENT_TITLES[tag.toLowerCase()];
  const tone = known?.tone ?? (/error|fail|fatal/i.test(tag) ? "danger" : "info");
  let title = known?.title ?? humanizeTag(tag);
  const checkpoint = /\(checkpoint (\d+)\)/.exec(rest);
  if (tag === "context_checkpoint" && checkpoint) title = `Context checkpoint ${checkpoint[1]}`;
  // A breadcrumb without key=value fields is prose: keep all of it.
  const detail = fields.length > 0 ? tail.trim() : rest.trim();
  return { tag, title, tone, fields, detail };
}

function humanizeTag(tag: string): string {
  const spaced = tag.replace(/[_-]+/g, " ").trim();
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

// ── the timeline ─────────────────────────────────────────────────────────────

export type ToolStep = {
  key: string;
  /** 1-based position among the run's tool calls. */
  step: number;
  name: string;
  argsRaw: string;
  resultRaw?: string;
  isError: boolean;
  pending?: boolean;
  /** Identical failed attempts folded into this one (live view). */
  repeats?: number;
  startedAt?: number;
  endedAt?: number;
};

export type TimelineItem =
  | { kind: "prompt"; key: string; members: number[]; msg: LogMessage }
  | { kind: "assistant"; key: string; members: number[]; msg: LogMessage; final: boolean }
  | { kind: "tool"; key: string; members: number[]; tool: ToolStep }
  | { kind: "event"; key: string; members: number[]; msg: LogMessage; event: RuntimeEvent }
  | { kind: "summary"; key: string; members: number[]; msg: LogMessage }
  | { kind: "subagent"; key: string; members: number[]; msg: LogMessage }
  | { kind: "user"; key: string; members: number[]; msg: LogMessage };

/**
 * buildTimeline folds a stored transcript into timeline items. Each tool call
 * becomes one step carrying its result (matched by tool_call_id), so the
 * result message is not rendered a second time; `members` lists every message
 * index an item stands for, which is what the filters count.
 */
export function buildTimeline(messages: LogMessage[]): TimelineItem[] {
  const resultIndex = new Map<string, number>();
  messages.forEach((m, i) => {
    if (m.role === "tool" && m.tool_call_id && !resultIndex.has(m.tool_call_id)) {
      resultIndex.set(m.tool_call_id, i);
    }
  });
  let lastAnswer = -1;
  messages.forEach((m, i) => {
    if (m.role === "assistant" && (m.content ?? "").trim()) lastAnswer = i;
  });

  const consumed = new Set<number>();
  const items: TimelineItem[] = [];
  let step = 0;
  messages.forEach((m, i) => {
    if (consumed.has(i)) return;
    const key = m.id ? `${m.id}-${i}` : `m${i}`;
    const role = m.role ?? "";
    if (m.message_type === "subagent_spawned") {
      items.push({ kind: "subagent", key, members: [i], msg: m });
      return;
    }
    if (m.message_type === "compaction_summary") {
      items.push({ kind: "summary", key, members: [i], msg: m });
      return;
    }
    if (role === "user") {
      if (i === 0) {
        items.push({ kind: "prompt", key, members: [i], msg: m });
        return;
      }
      const event = parseRuntimeEvent(m.content ?? "");
      if (event) items.push({ kind: "event", key, members: [i], msg: m, event });
      else items.push({ kind: "user", key, members: [i], msg: m });
      return;
    }
    if (role === "assistant") {
      if ((m.content ?? "").trim() || (m.reasoning ?? "").trim()) {
        items.push({ kind: "assistant", key, members: [i], msg: m, final: i === lastAnswer });
      }
      (m.tool_calls ?? []).forEach((tc: LogToolCall, j) => {
        step += 1;
        const shown = unwrapDeferredCall(tc.name, tc.arguments);
        const ri = tc.id ? resultIndex.get(tc.id) : undefined;
        const result = ri !== undefined ? messages[ri] : undefined;
        if (ri !== undefined) consumed.add(ri);
        items.push({
          kind: "tool",
          key: `${key}-c${j}`,
          members: ri !== undefined ? [i, ri] : [i],
          tool: {
            key: `${key}-c${j}`,
            step,
            name: shown.name,
            argsRaw: shown.input,
            resultRaw: result?.content,
            isError: Boolean(result?.is_error),
            startedAt: m.created_at,
            endedAt: result?.created_at,
          },
        });
      });
      return;
    }
    if (role === "tool") {
      // A result whose call is missing (truncated log): show it on its own.
      step += 1;
      items.push({
        kind: "tool",
        key,
        members: [i],
        tool: {
          key,
          step,
          name: m.tool_name || "tool",
          argsRaw: "",
          resultRaw: m.content,
          isError: Boolean(m.is_error),
          endedAt: m.created_at,
        },
      });
      return;
    }
    items.push({ kind: "user", key, members: [i], msg: m });
  });
  return items;
}

/** Whether a tool step ended in failure, by every signal parseToolResult reads. */
export function stepFailed(tool: ToolStep): boolean {
  if (tool.isError) return true;
  if (tool.resultRaw === undefined) return false;
  return !parseToolResult(tool.name, tool.resultRaw, tool.isError).ok;
}

/** Lower-cased text an item is searched by. */
export function itemSearchText(item: TimelineItem): string {
  switch (item.kind) {
    case "tool":
      return `${item.tool.name}\n${item.tool.argsRaw}\n${item.tool.resultRaw ?? ""}`.toLowerCase();
    case "assistant":
      return `${item.msg.content ?? ""}\n${item.msg.reasoning ?? ""}`.toLowerCase();
    default:
      return (item.msg.content ?? "").toLowerCase();
  }
}
