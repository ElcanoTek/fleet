// Pure functions shared with chat-experience.tsx — extracted so they can be
// unit-tested under vitest without pulling in React/Next.js.
//
// Keep this file side-effect-free. If you're tempted to add a DOM ref, a
// fetch, or a component, that belongs in chat-experience.tsx instead.

export type ToolCallState = "pending" | "done" | "error";

export type ToolCall = {
  id: string;
  name: string;
  input: string;
  resultText?: string;
  state: ToolCallState;
};

export type PythonStream = {
  /** Parsed stdout from the run_python bridge, or the raw tool text when parsing fails. */
  stdout: string;
  /** Captured stderr. Rendered distinctly (red) when present. */
  stderr?: string;
  /** Non-zero when the bridge returned an `error` field. */
  error?: string;
  /** Kernel execution time in ms. Surfaced in the footer chip. */
  executionMs?: number;
};

/**
 * parsePythonStream extracts the human-readable parts of a run_python
 * tool result. The bridge script wraps output in a JSON envelope — we
 * don't want to splat that at the user. Fall back to the raw text if
 * the envelope isn't parsable for any reason.
 */
export function parsePythonStream(raw: string): PythonStream {
  try {
    const parsed = JSON.parse(raw) as {
      stdout?: string;
      stderr?: string;
      output?: string;
      error?: string;
      execution_time_ms?: number;
    };
    return {
      stdout: String(parsed.stdout ?? parsed.output ?? ""),
      stderr: parsed.stderr ? String(parsed.stderr) : undefined,
      error: parsed.error ? String(parsed.error) : undefined,
      executionMs: typeof parsed.execution_time_ms === "number" ? parsed.execution_time_ms : undefined,
    };
  } catch {
    return { stdout: raw };
  }
}

export type MessageState = "thinking" | "streaming" | "done";

export type ApprovalStatus = "pending" | "approved" | "rejected" | "failed";

export type MemoryProposalStatus = "pending" | "saved" | "dismissed";

export type MemoryProposal = {
  id: string;
  content: string;
  status: MemoryProposalStatus;
};

export type Approval = {
  id: string;
  tool: string;
  summary: {
    // email
    to?: string | string[];
    cc?: string | string[];
    bcc?: string | string[];
    subject?: string;
    from?: string;
    /** Truncated plain-string snippet for lightweight inline display. */
    preview?: string;
    /** Full (capped at 1 MiB server-side) body used for the rendered preview. */
    content?: string;
    /** "text/html" or "text/plain" — drives the preview render path. */
    content_type?: string;
    /** true when the server truncated `content` because the payload exceeded the preview cap. */
    content_overflow?: boolean;

    // bash
    command?: string;
    working_dir?: string;
    timeout_seconds?: number;

    // suggest_advanced_model
    /** One-line agent-supplied rationale rendered on the suggestion card. */
    reason?: string;
    /** Server-authoritative slug the conversation will be pinned to on accept. */
    recommend_model?: string;
  };
  status: ApprovalStatus;
  resultText?: string;
};

/**
 * Status of an external-agent permission request. "pending" while awaiting the
 * human; "allowed"/"denied" after the human (or the default-deny timeout)
 * decides. There is intentionally no "approve all" state — each request is
 * decided on its own.
 */
export type PermissionStatus = "pending" | "allowed" | "denied";

/** One option an external agent offered for a permission request. */
export type PermissionOption = {
  optionId: string;
  name: string;
  /** ACP option kind: "allow_once" / "reject_once" / … — drives button shape. */
  kind: string;
};

/**
 * PermissionRequest is an EXTERNAL ACP agent's session/request_permission
 * surfaced to the human as an inline allow/deny prompt. The external agent
 * (Claude Code / Goose) self-executes in a locked sandbox; when it wants to do
 * something sensitive it asks, and fleet blocks its turn on the human's
 * decision (default-deny on timeout). The user picks an option (or denies) and
 * the decision is POSTed back; the agent's turn then continues.
 */
export type PermissionRequest = {
  id: string;
  /** Human-readable description of what the agent wants to do. */
  title: string;
  /** ACP tool kind (edit/read/execute/…) for an icon/treatment, if provided. */
  kind?: string;
  /** File paths the action touches, for the human to review. */
  locations?: string[];
  /** The agent's offered choices. */
  options: PermissionOption[];
  status: PermissionStatus;
};

/** Per-turn cost + tokens + duration for the inline chip under assistant messages. */
export type TurnSummary = {
  costUsd: number;
  /**
   * Sum of `usage.InputTokens` across every model call (step) within this
   * turn. Load-bearing for cost telemetry (OpenRouter bills per-step input
   * tokens) and for the cached-percentage chip (cachedTokens / promptTokens).
   *
   * NOT the right denominator for "how full is the model's context window."
   * A 9-step agentic turn easily reports 800k+ here even though no single
   * step ever sent more than ~200k. Use `promptTokensLastStep` for the
   * context-usage indicator.
   */
  promptTokens: number;
  /**
   * Input-token count for the FINAL step of this turn — the most honest
   * "fraction of context window used" signal. Added after a production
   * incident where the indicator divided the (summed) `promptTokens` by
   * context_length and reported impossible >100% fractions on tool-heavy
   * turns. Optional because older persisted summaries don't have it; UI
   * falls back to `promptTokens` with a defensive 100% clamp in that case.
   */
  promptTokensLastStep?: number;
  completionTokens: number;
  cachedTokens: number;
  cacheCreationTokens: number;
  durationMs: number;
  cancelled?: boolean;
  /** OpenRouter slug that actually ran this turn, e.g. "anthropic/claude-sonnet-4.6". */
  model?: string;
};

/**
 * cachedPercent returns the cache-hit rate for a turn as an integer percent
 * [0, 100]. OpenRouter's `prompt_tokens` already includes `cached_tokens`
 * and `cache_write_tokens` — it's the full input size, not a complement —
 * so the denominator is `promptTokens` alone. Adding cachedTokens back in
 * would double-count and always understate the hit rate.
 *
 * Uses Math.floor, not Math.round: the latest user message is never cached,
 * so a real warm turn tops out around 99.x% and rounding that to "100%
 * cached" looks like a bug. Floor keeps 100% reserved for the defensive
 * cached>=prompt case.
 *
 * Returns null when there are no prompt tokens yet (e.g. a cancelled turn
 * that never reached the model) so callers can hide the chip.
 */
export function cachedPercent(summary: Pick<TurnSummary, "promptTokens" | "cachedTokens">): number | null {
  if (summary.promptTokens <= 0) return null;
  const capped = Math.min(summary.cachedTokens, summary.promptTokens);
  return Math.floor((capped / summary.promptTokens) * 100);
}

/** Aggregate cost + token-weighted cache-hit rate across a conversation. */
export type ConversationTotals = {
  costUsd: number;
  promptTokens: number;
  cachedTokens: number;
  turns: number;
  cachedPercent: number | null;
};

/**
 * conversationTotals sums cost/tokens across every persisted turn summary.
 * Cache-hit % is token-weighted (sum(cached) / sum(prompt)) rather than a
 * mean-of-percents — a single 17k-token turn should dwarf a 40-token one in
 * the conversation-level signal.
 */
export function conversationTotals(summaries: readonly TurnSummary[]): ConversationTotals {
  let costUsd = 0;
  let promptTokens = 0;
  let cachedTokens = 0;
  let turns = 0;
  for (const s of summaries) {
    costUsd += s.costUsd;
    promptTokens += s.promptTokens;
    cachedTokens += Math.min(s.cachedTokens, s.promptTokens);
    turns += 1;
  }
  const pct = promptTokens > 0 ? Math.floor((cachedTokens / promptTokens) * 100) : null;
  return { costUsd, promptTokens, cachedTokens, turns, cachedPercent: pct };
}

/**
 * shortModelName trims the provider prefix from an OpenRouter slug so the
 * chip stays scannable on mobile ("claude-sonnet-4.6" vs.
 * "anthropic/claude-sonnet-4.6"). Returns the input unchanged if it has no
 * provider segment.
 */
export function shortModelName(slug: string | undefined): string {
  if (!slug) return "";
  const slash = slug.indexOf("/");
  return slash >= 0 ? slug.slice(slash + 1) : slug;
}

/**
 * RetryNotice accompanies a message whose provider call is currently being
 * retried by the server-side fantasy SDK. Non-terminal: the turn may still
 * succeed, fail, or end up asking the user to pick a different model.
 * Cleared as soon as forward progress resumes (text.delta / tool.call) or
 * a terminal event arrives.
 */
export type RetryNotice = {
  /** HTTP status the provider returned on the attempt that prompted the retry. */
  statusCode: number;
  /** Short title from the provider error ("too many requests", "overloaded"). */
  title: string;
  /** Longer message from the provider, suitable for a tooltip. */
  message: string;
  /** Server-computed delay before the next attempt, in milliseconds. */
  delayMs: number;
};

/**
 * ModelSelectionReason is the server-classified cause of a
 * `turn.model_required` event. Kept in sync with the Go
 * `ModelSelectionReason` constants in
 * server/internal/agent/resilience.go.
 */
export type ModelSelectionReason = "context_too_large" | "retry_exhausted" | "fatal";

/**
 * ModelRequired is set on an assistant message when the server decided the
 * current model cannot complete this turn and the user needs to pick a
 * different one. Terminal: the turn is done, but the UI surfaces a
 * model-picker affordance instead of a generic error.
 */
export type ModelRequired = {
  reason: ModelSelectionReason;
  /** The OpenRouter slug that failed (e.g. "anthropic/claude-sonnet-4.6"). */
  failedModel: string;
  /** HTTP status code that drove the classification; 0 for non-provider errors. */
  statusCode: number;
  /** Human-readable explanation from the server, ready to display. */
  message: string;
};

/**
 * SummaryMeta is attached to messages with `kind === "summary"` — the
 * compaction markers inserted by the user-initiated "summarize and
 * continue" flow. The chip surfaces who summarized + how much it cost.
 */
export type SummaryMeta = {
  model?: string;
  promptTokens?: number;
  completionTokens?: number;
  costUsd?: number;
};

export type Message = {
  id: number;
  role: "assistant" | "user";
  /**
   * Optional discriminator. Absent (or "text") means a normal turn.
   * "summary" means the message is a compaction marker — the renderer
   * draws a distinct banner and pre-summary messages collapse behind
   * a "+ N earlier turns" expander.
   */
  kind?: "text" | "summary";
  content: string;
  state: MessageState;
  reasoning?: string;
  toolCalls?: ToolCall[];
  pythonStreams?: PythonStream[];
  approvals?: Approval[];
  memoryProposals?: MemoryProposal[];
  /** External-agent permission prompts surfaced inline (allow/deny). */
  permissionRequests?: PermissionRequest[];
  summary?: TurnSummary;
  /** Populated when kind === "summary" — drives the summary banner chip. */
  summaryMeta?: SummaryMeta;
  /** true when the user stopped the turn mid-stream. */
  cancelled?: boolean;
  /** true when the turn ended with a server error. Distinct from cancelled. */
  failed?: boolean;
  /** Set while the server is retrying a transient failure; cleared on forward progress. */
  retrying?: RetryNotice;
  /** Set when the server asks the user to pick a different model. Terminal. */
  modelRequired?: ModelRequired;
};

/**
 * RetryEventPayload is the JSON shape of a `turn.retry` SSE event. The
 * server emits these from fantasy's OnRetry callback — they are
 * informational, not terminal.
 */
export type RetryEventPayload = {
  status_code?: number;
  title?: string;
  message?: string;
  delay_ms?: number;
};

/**
 * ModelRequiredEventPayload is the JSON shape of a `turn.model_required`
 * SSE event. Terminal: the turn is done and the UI should reopen its
 * model picker.
 */
export type ModelRequiredEventPayload = {
  reason?: string;
  failed_model?: string;
  status_code?: number;
  message?: string;
  raw?: string;
};

const modelSelectionReasons: readonly ModelSelectionReason[] = [
  "context_too_large",
  "retry_exhausted",
  "fatal",
];

function normaliseReason(raw: unknown): ModelSelectionReason {
  if (typeof raw === "string" && (modelSelectionReasons as readonly string[]).includes(raw)) {
    return raw as ModelSelectionReason;
  }
  return "fatal";
}

/**
 * applyRetryNotice marks a message as currently retrying. Pure so it can
 * be unit-tested without the component tree. The caller is responsible
 * for resetting the notice on the next forward-progress event.
 */
export function applyRetryNotice(message: Message, payload: RetryEventPayload): Message {
  return {
    ...message,
    retrying: {
      statusCode: typeof payload.status_code === "number" ? payload.status_code : 0,
      title: typeof payload.title === "string" ? payload.title : "",
      message: typeof payload.message === "string" ? payload.message : "",
      delayMs: typeof payload.delay_ms === "number" ? payload.delay_ms : 0,
    },
  };
}

/**
 * clearRetryNotice removes the retrying flag when forward progress resumes.
 * Safe to call on messages without a prior retry; returns the original
 * reference in that case to keep React reconciliation cheap.
 */
export function clearRetryNotice(message: Message): Message {
  if (!message.retrying) return message;
  const { retrying: _retrying, ...rest } = message;
  return rest;
}

/**
 * applyModelRequired resolves a `turn.model_required` event onto the
 * assistant message: marks the turn done, flags it failed so existing
 * rendering stays consistent, and stashes the server's reason + human
 * copy for the model-picker banner.
 *
 * Also clears the retrying notice: turn.model_required supersedes any
 * in-flight retry (we've given up on this model).
 */
export function applyModelRequired(message: Message, payload: ModelRequiredEventPayload): Message {
  const humanMessage =
    typeof payload.message === "string" && payload.message.length > 0
      ? payload.message
      : "Pick a different model to continue.";
  const { retrying: _retrying, ...rest } = message;
  return {
    ...rest,
    // Keep any streamed content so the user can still read whatever
    // the model produced before it gave up; fall back to the human
    // message when nothing came through.
    content: message.content || humanMessage,
    state: "done",
    failed: true,
    modelRequired: {
      reason: normaliseReason(payload.reason),
      failedModel: typeof payload.failed_model === "string" ? payload.failed_model : "",
      statusCode: typeof payload.status_code === "number" ? payload.status_code : 0,
      message: humanMessage,
    },
  };
}

export type HistoryEntry = {
  role: "user" | "assistant" | "tool";
  type: "text" | "reasoning" | "tool_call" | "tool_result" | "turn_summary" | "summary";
  content: Record<string, unknown>;
};

/**
 * historyToMessages replays a chat-server history (flat list of event rows)
 * into the UI's grouped Message shape.
 *
 * Invariants:
 *   - A user `text` row always opens a new Message.
 *   - Assistant `text`, `reasoning`, `tool_call`, and `tool_result` rows are
 *     coalesced into the current assistant Message; if none is open we start
 *     one so orphan rows still render.
 *   - Tool results match back to their Tool call by id; a missing tool call
 *     does NOT create one retroactively (keeps parity with the live stream).
 *   - run_python results additionally become a pythonStreams entry so the
 *     UI renders them in a monospace block.
 */
export function historyToMessages(entries: HistoryEntry[]): Message[] {
  const messages: Message[] = [];
  let current: Message | null = null;
  let nextId = 1;

  // Tool results whose id matched no tool_call in the message that was open
  // when we processed them. Most are genuine orphans (dropped — see the
  // "no retroactive creation" invariant). But an approval resolution
  // (send_email / bash) is appended out-of-band with the ORIGINAL tool_call
  // id when the user clicks Send: if they click before the turn that issued
  // the call has been persisted, that resolution row lands BEFORE its own
  // tool_call in id order. A single forward pass would drop it and the chip
  // would stay stuck on "APPROVAL_REQUIRED" even though the send succeeded
  // ("looks like nothing happened"). We re-apply these against tool_calls in
  // ANY message in a post-pass below.
  const orphanResults: Array<{ id: string; name: string; text: string; is_err: boolean }> = [];

  const flush = () => {
    if (current) {
      messages.push(current);
      current = null;
    }
  };

  for (const e of entries) {
    if (e.type === "text" && e.role === "user") {
      flush();
      messages.push({
        id: nextId++,
        role: "user",
        content: String((e.content as { text?: string }).text ?? ""),
        state: "done",
      });
      continue;
    }
    if (e.type === "text" && e.role === "assistant") {
      if (!current) {
        current = { id: nextId++, role: "assistant", content: "", state: "done" };
      }
      current.content += String((e.content as { text?: string }).text ?? "");
      continue;
    }
    if (e.type === "reasoning") {
      if (!current) {
        current = { id: nextId++, role: "assistant", content: "", state: "done" };
      }
      const txt = String((e.content as { text?: string }).text ?? "");
      current.reasoning = (current.reasoning ?? "") + (current.reasoning ? "\n" : "") + txt;
      continue;
    }
    if (e.type === "tool_call") {
      if (!current) {
        current = { id: nextId++, role: "assistant", content: "", state: "done" };
      }
      const c = e.content as { id: string; name: string; input: string };
      const tc: ToolCall = { id: c.id, name: c.name, input: c.input, state: "done" };
      current.toolCalls = [...(current.toolCalls ?? []), tc];
      continue;
    }
    if (e.type === "summary") {
      // Compaction marker — flush whatever assistant turn was in
      // flight and emit the summary as its own banner-style message
      // so the UI can render a distinct chrome (and collapse the
      // pre-summary scroll behind a "+ N earlier turns" expander).
      flush();
      const c = e.content as {
        text?: string;
        model?: string;
        prompt_tokens?: number;
        completion_tokens?: number;
        cost_usd?: number;
      };
      messages.push({
        id: nextId++,
        role: "assistant",
        kind: "summary",
        content: String(c.text ?? ""),
        state: "done",
        summaryMeta: {
          model: c.model,
          promptTokens: c.prompt_tokens,
          completionTokens: c.completion_tokens,
          costUsd: c.cost_usd,
        },
      });
      continue;
    }
    if (e.type === "turn_summary") {
      if (!current) {
        current = { id: nextId++, role: "assistant", content: "", state: "done" };
      }
      const c = e.content as {
        cost_usd?: number;
        prompt_tokens?: number;
        prompt_tokens_last_step?: number;
        completion_tokens?: number;
        cached_tokens?: number;
        cache_creation_tokens?: number;
        duration_ms?: number;
        cancelled?: boolean;
        model?: string;
      };
      current.summary = {
        costUsd: c.cost_usd ?? 0,
        promptTokens: c.prompt_tokens ?? 0,
        promptTokensLastStep: c.prompt_tokens_last_step,
        completionTokens: c.completion_tokens ?? 0,
        cachedTokens: c.cached_tokens ?? 0,
        cacheCreationTokens: c.cache_creation_tokens ?? 0,
        durationMs: c.duration_ms ?? 0,
        cancelled: c.cancelled,
        model: c.model,
      };
      continue;
    }

    if (e.type === "tool_result") {
      if (!current) {
        current = { id: nextId++, role: "assistant", content: "", state: "done" };
      }
      const c = e.content as { id: string; name: string; text: string; is_err: boolean };
      if ((current.toolCalls ?? []).some((t) => t.id === c.id)) {
        current.toolCalls = (current.toolCalls ?? []).map((t) =>
          t.id === c.id ? { ...t, resultText: c.text, state: c.is_err ? "error" : "done" } : t,
        );
      } else {
        // No matching call in the open message — defer to the post-pass,
        // which resolves it against tool_calls in any message (or drops it
        // if there's genuinely no matching call anywhere).
        orphanResults.push(c);
      }
      if (c.name === "run_python" && c.text) {
        current.pythonStreams = [
          ...(current.pythonStreams ?? []),
          parsePythonStream(c.text),
        ];
      }
      continue;
    }
  }
  flush();

  // Resolve out-of-order tool results (see orphanResults above). Scan every
  // message's tool calls for the matching id and overwrite its result —
  // an appended approval resolution is the authoritative final outcome, so
  // it wins over the inline "APPROVAL_REQUIRED" placeholder the call landed
  // with. Results that match no call anywhere stay dropped, preserving the
  // "no retroactive creation" invariant.
  for (const c of orphanResults) {
    for (const m of messages) {
      if (!m.toolCalls) continue;
      let matched = false;
      m.toolCalls = m.toolCalls.map((t) => {
        if (t.id !== c.id) return t;
        matched = true;
        return { ...t, resultText: c.text, state: c.is_err ? "error" : "done" };
      });
      if (matched) break;
    }
  }

  return messages;
}

/** Map an MCP tool name like `mcp_email_search_emails` to a display string. */
export function prettyToolName(name: string): string {
  if (name.startsWith("mcp_email_")) return name.replace("mcp_email_", "email: ");
  if (name.startsWith("mcp_sendgrid_")) return name.replace("mcp_sendgrid_", "sendgrid: ");
  if (name.startsWith("mcp_")) return name.replace(/^mcp_/, "");
  return name;
}

function humanize(token: string): string {
  const spaced = token.replace(/_/g, " ").trim();
  if (!spaced) return spaced;
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

// SSP MCPs follow a `mcp_<server>_<short_prefix>_<rest>` pattern where
// the short prefix duplicates the server name (e.g.
// `mcp_pubmatic_pm_run_standard_report`,
// `mcp_indexexchange_ix_list_report_files`). Stripping both prefixes
// and prepending a clean display name keeps the indicator readable —
// without this we get "Pubmatic pm run standard report" which is
// triple-redundant.
const sspPrefixes: Array<{ match: string; tools: string[]; display: string }> = [
  { match: "mcp_pubmatic_", tools: ["pm_"], display: "PubMatic" },
  { match: "mcp_indexexchange_", tools: ["ix_"], display: "Index Exchange" },
  { match: "mcp_magnite_", tools: ["magnite_"], display: "Magnite" },
  { match: "mcp_xandr_", tools: ["xandr_"], display: "Xandr" },
  { match: "mcp_medianet_", tools: ["mn_"], display: "Media.net" },
];

/**
 * Human-readable, present-progressive label for the thinking indicator.
 * Distinct from `prettyToolName` (used in chips) because the indicator sits
 * next to a natural-language task title and should read like prose.
 */
export function humanToolLabel(name: string): string {
  switch (name) {
    case "view_file":
      return "Reading file";
    case "edit_file":
      return "Editing file";
    case "write_file":
      return "Writing file";
    case "run_python":
      return "Running Python";
    case "bash":
      return "Running shell";
    case "task_tracker":
      return "Updating plan";
    case "web_fetch":
      return "Fetching page";
    case "smart_search":
      return "Searching the web";
    case "mcp_email_send_email":
      return "Sending email";
    case "mcp_email_search_emails":
      return "Searching inbox";
  }
  for (const { match, tools, display } of sspPrefixes) {
    if (!name.startsWith(match)) continue;
    let rest = name.slice(match.length);
    for (const tp of tools) {
      if (rest.startsWith(tp)) {
        rest = rest.slice(tp.length);
        break;
      }
    }
    rest = rest.replace(/_/g, " ").trim();
    return rest ? `${display} ${rest}` : display;
  }
  if (name.startsWith("mcp_email_")) {
    return `Email ${humanize(name.slice("mcp_email_".length)).toLowerCase()}`;
  }
  if (name.startsWith("mcp_sendgrid_")) {
    return `SendGrid ${humanize(name.slice("mcp_sendgrid_".length)).toLowerCase()}`;
  }
  if (name.startsWith("mcp_")) {
    return humanize(name.slice("mcp_".length));
  }
  return humanize(name);
}

/** Single-emoji glyph for a tool, picked for scannability in dense chips. */
export function toolIcon(name: string): string {
  if (name === "run_python") return "🐍";
  if (name === "bash") return "❯";
  if (name === "task_tracker") return "✓";
  if (name === "view_file" || name === "write_file" || name === "edit_file") return "📄";
  if (name === "web_fetch" || name === "smart_search") return "🔎";
  if (name.startsWith("mcp_email_")) return "📧";
  if (name.startsWith("mcp_sendgrid_")) return "📤";
  return "🛠";
}

/**
 * safePretty pretty-prints a JSON string; falls back to the raw string if
 * parsing fails. Used to format tool-call input JSON in the expanded chip.
 */
export function safePretty(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
