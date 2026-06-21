// Helpers for building Jules API requests from a conversation export.
//
// Jules only sees what's in the prompt and what it can read from the
// connected GitHub repo. The chat transcript IS the bug report, so we send
// it in full — every user/assistant message, every reasoning trace, every
// tool call AND tool result body — and only clip when the chat is so long
// that it pushes the request past Jules' (undocumented) request-body cap.
//
// Jules' API docs do not publish a max body size. Empirically the platform
// accepts JSON bodies up through ~1.7 MB and rejects 1.9 MB+ with HTTP 400
// "invalid argument" (5 MB is also rejected). 1.5 MB leaves ~12% headroom
// under that observed ceiling and a few KB more for the JSON envelope
// (sourceContext, title, headers). When a chat is bigger than this we
// keep BOTH the opening turns (where the user states what they're trying
// to accomplish) AND the most recent turns (where things broke), and
// elide the middle — see clipTranscript.

const DEFAULT_MAX_PROMPT_BYTES = 1_500_000;

export type ExportEnvelope = {
  conversation?: {
    id?: string;
    title?: string;
    persona?: string;
    model?: string;
    created_at?: number;
    updated_at?: number;
    lockdown?: boolean;
    optional_mcp_servers_enabled?: string[] | null;
  };
  history?: HistoryEntry[];
  exported_at?: string;
};

export type HistoryEntry = {
  role?: string;
  type?: string;
  content?: unknown;
};

export type BuildPromptInput = {
  userEmail: string;
  note?: string;
  exportEnvelope: ExportEnvelope;
  appUrl?: string;
  buildId?: string;
  maxPromptBytes?: number;
};

export type BuildPromptOutput = {
  prompt: string;
  title: string;
  truncated: boolean;
  messageCount: number;
};

export function buildJulesPrompt(input: BuildPromptInput): BuildPromptOutput {
  const {
    userEmail,
    note,
    exportEnvelope,
    appUrl,
    buildId,
    maxPromptBytes = DEFAULT_MAX_PROMPT_BYTES,
  } = input;

  const conv = exportEnvelope.conversation ?? {};
  const history = Array.isArray(exportEnvelope.history) ? exportEnvelope.history : [];
  const transcript = formatHistory(history);

  const optionalMcp = Array.isArray(conv.optional_mcp_servers_enabled)
    ? conv.optional_mcp_servers_enabled.filter((n): n is string => typeof n === "string")
    : [];

  const header = [
    "A user of the Elcano chat app filed a bug report from inside the chat UI.",
    "They were having trouble with the conversation transcribed below and want a fix.",
    "",
    "Please:",
    "  1. Read the transcript end-to-end and figure out what went wrong from the user's",
    "     point of view — where was the model unhelpful, what did it miss, where did the",
    "     UI/agent surprise the user, what tool call returned the wrong thing?",
    "  2. Trace the issue back to a concrete cause in this repo. The frontend is Next.js",
    "     under `src/` (chat UI in `src/app/ui/chat-experience.tsx`, API proxies in",
    "     `src/app/api/`); the agent harness is Go under `server/` (HTTP layer in",
    "     `server/internal/httpapi/`, agent loop in `server/internal/agent/`); MCP tool",
    "     servers live under `server/mcp_servers/`. The transcript's tool calls/results",
    "     name the exact tools so you can grep their handlers.",
    "  3. Implement the smallest fix that addresses the root cause and open a pull",
    "     request against the `dev` branch. If you need clarification, leave it as a",
    "     comment in the PR — do not ask the reporter directly.",
    "",
    "── Report metadata ──",
    `Reporter: ${userEmail}`,
    `Conversation id: ${conv.id ?? "(unknown)"}`,
    conv.title ? `Conversation title: ${conv.title}` : null,
    conv.persona ? `Persona: ${conv.persona}` : null,
    conv.model ? `Model in use: ${conv.model}` : null,
    typeof conv.lockdown === "boolean" ? `Lockdown chat: ${conv.lockdown}` : null,
    optionalMcp.length ? `Optional MCP servers enabled: ${optionalMcp.join(", ")}` : null,
    appUrl ? `App URL: ${appUrl}` : null,
    buildId ? `App build id: ${buildId}` : null,
    typeof conv.created_at === "number" ? `Created at: ${formatUnix(conv.created_at)}` : null,
    typeof conv.updated_at === "number" ? `Last updated: ${formatUnix(conv.updated_at)}` : null,
    `Messages in transcript: ${history.length}`,
    note && note.trim() ? `\nReporter note:\n${note.trim()}` : null,
    "",
    "── Conversation transcript (full content; tool inputs and outputs included verbatim) ──",
  ]
    .filter((line): line is string => line !== null)
    .join("\n");

  const footer = "\n── End transcript ──";

  const budget = Math.max(2_000, maxPromptBytes - header.length - footer.length);
  const { body, truncated } = clipTranscript(transcript, budget);

  const prompt = `${header}\n${body}${footer}`;
  const title = `Bug report: ${conv.title || conv.id || "untitled chat"}`.slice(0, 200);

  return {
    prompt,
    title,
    truncated,
    messageCount: history.length,
  };
}

// formatHistory renders the chat-server's HistoryEntry stream into a flat
// human-readable transcript. We keep the FULL content — tool inputs, tool
// outputs, reasoning — because that's exactly the surface area where a bug
// usually hides. Clipping happens later, in clipTranscript, only when the
// total transcript would push the request past Jules' body cap.
export function formatHistory(history: HistoryEntry[]): string {
  const parts: string[] = [];
  for (const entry of history) {
    const role = (entry.role ?? "").toLowerCase();
    const type = (entry.type ?? "").toLowerCase();
    const content = entry.content as Record<string, unknown> | null | undefined;

    if (type === "text") {
      const text = stringField(content, "text");
      if (!text) continue;
      const tag = role === "user" ? "USER" : role === "assistant" ? "ASSISTANT" : role.toUpperCase();
      const images = imagesSummary(content);
      parts.push(`[${tag}]${images}\n${text.trim()}`);
      continue;
    }
    if (type === "reasoning") {
      const text = stringField(content, "text");
      if (!text) continue;
      parts.push(`[ASSISTANT (reasoning)]\n${text.trim()}`);
      continue;
    }
    if (type === "tool_call") {
      const name = stringField(content, "name") || "tool";
      const id = stringField(content, "id");
      const args = jsonField(content, "input") || jsonField(content, "arguments");
      const head = id ? `[TOOL CALL] ${name} (id=${id})` : `[TOOL CALL] ${name}`;
      parts.push(args ? `${head}\nArguments:\n${args}` : head);
      continue;
    }
    if (type === "tool_result") {
      const id = stringField(content, "id") || stringField(content, "tool_call_id");
      const text =
        stringField(content, "text") ||
        stringField(content, "output") ||
        jsonField(content, "result") ||
        "";
      const head = id ? `[TOOL RESULT] (id=${id})` : `[TOOL RESULT]`;
      parts.push(text ? `${head}\n${text}` : `${head}\n(empty)`);
      continue;
    }
  }
  return parts.join("\n\n");
}

// clipTranscript keeps both the OPENING turns (the user states their goal)
// and the RECENT turns (where things went sideways) when we can't fit the
// whole transcript, and elides the middle. This beats a plain tail-keep:
// without the first few turns Jules has to guess what the user was trying
// to accomplish, which is exactly the diagnostic context a bug report needs.
//
// Budget split: ~25% head, ~75% tail. Both ends snap to turn boundaries
// (`\n\n[`) so we never start or end mid-message. If the budget is so
// small that we can't fit a full turn at both ends, we fall back to
// keeping the tail only — recent turns beat half-turns.
export function clipTranscript(text: string, maxBytes: number): { body: string; truncated: boolean } {
  if (text.length <= maxBytes) return { body: text, truncated: false };

  const headBudget = Math.floor(maxBytes * 0.25);
  const tailBudget = maxBytes - headBudget;

  const headEnd = lastTurnBoundaryWithin(text, headBudget);
  const tailStart = firstTurnBoundaryFromEnd(text, tailBudget);

  // Head+tail succeeded only when (a) we found at least one turn boundary
  // for the head, (b) we found at least one usable turn boundary for the
  // tail (i.e. the tail isn't empty), and (c) the two halves don't
  // overlap. Anything else → tail-only fallback.
  const headOk = headEnd > 0;
  const tailOk = tailStart < text.length;
  const noOverlap = tailStart > headEnd;

  if (headOk && tailOk && noOverlap) {
    const head = text.slice(0, headEnd);
    const tail = text.slice(tailStart);
    const elided = tailStart - headEnd;
    const elision = `\n\n[…${elided.toLocaleString("en-US")} characters of the middle of the transcript elided to fit Jules' prompt budget — opening and recent turns kept verbatim…]\n\n`;
    return { body: `${head}${elision}${tail}`, truncated: true };
  }

  // Fallback: keep the most recent maxBytes, snapped to a turn boundary
  // when we can find one — otherwise return the raw character tail.
  const tail = text.slice(text.length - maxBytes);
  const boundary = tail.indexOf("\n\n[");
  const start = boundary >= 0 ? boundary + 2 : 0;
  const elidedHead = text.length - tail.length + start;
  return {
    body: `(earlier ${elidedHead.toLocaleString("en-US")} characters of the transcript were elided to fit Jules' prompt budget)\n\n${tail.slice(start)}`,
    truncated: true,
  };
}

// Largest end-position ≤ budget that sits exactly on a "\n\n[" turn
// boundary (or end-of-string), so the head ends cleanly between turns.
function lastTurnBoundaryWithin(text: string, budget: number): number {
  if (budget >= text.length) return text.length;
  let end = -1;
  let from = 0;
  while (true) {
    const next = text.indexOf("\n\n[", from);
    if (next < 0 || next > budget) break;
    end = next;
    from = next + 2;
  }
  return end < 0 ? 0 : end;
}

// Smallest start-position such that text.slice(start) is ≤ budget AND
// `start` sits exactly on a "\n\n[" boundary (so the tail begins with a
// clean turn header).
function firstTurnBoundaryFromEnd(text: string, budget: number): number {
  const minStart = Math.max(0, text.length - budget);
  const next = text.indexOf("\n\n[", minStart);
  return next < 0 ? text.length : next + 2;
}

function stringField(obj: Record<string, unknown> | null | undefined, key: string): string {
  if (!obj) return "";
  const v = obj[key];
  return typeof v === "string" ? v : "";
}

function jsonField(obj: Record<string, unknown> | null | undefined, key: string): string {
  if (!obj) return "";
  const v = obj[key];
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return "";
  }
}

function formatUnix(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "(unknown)";
  try {
    return new Date(seconds * 1000).toISOString();
  } catch {
    return "(unknown)";
  }
}

function imagesSummary(content: Record<string, unknown> | null | undefined): string {
  if (!content) return "";
  const images = content["images"];
  if (!Array.isArray(images) || images.length === 0) return "";
  const names = images
    .map((img) => {
      if (!img || typeof img !== "object") return null;
      const obj = img as Record<string, unknown>;
      return (typeof obj.name === "string" && obj.name) ||
        (typeof obj.path === "string" && obj.path) ||
        null;
    })
    .filter((s): s is string => Boolean(s));
  return ` (${images.length} attached image${images.length === 1 ? "" : "s"}${names.length ? ": " + names.join(", ") : ""})`;
}

export function getJulesConfig() {
  // `??` only catches undefined/null, but the provisioning template
  // ships JULES_API_KEY="" / JULES_SOURCE="" / JULES_BRANCH="" as
  // unset-by-default placeholders. Treat empty strings the same as
  // missing so an operator clearing a value falls back to the default.
  const apiKey = nonEmpty(process.env.JULES_API_KEY);
  const baseUrl = (nonEmpty(process.env.JULES_API_BASE) ?? "https://jules.googleapis.com/v1alpha").replace(
    /\/+$/,
    "",
  );
  const source = nonEmpty(process.env.JULES_SOURCE) ?? "sources/github/ElcanoTek/chat";
  const branch = nonEmpty(process.env.JULES_BRANCH) ?? "dev";
  return { apiKey, baseUrl, source, branch };
}

function nonEmpty(value: string | undefined): string | undefined {
  if (value === undefined || value === null) return undefined;
  const trimmed = value.trim();
  return trimmed === "" ? undefined : trimmed;
}
