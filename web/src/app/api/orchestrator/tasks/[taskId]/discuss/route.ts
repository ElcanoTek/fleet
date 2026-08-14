import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";
import { orchestratorFetch } from "@/app/lib/orchestratorServer";
import { resolveOrchestratorAuth } from "../../../_lib/auth";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// "Discuss this run" (docs/DISCUSS-RUN.md): POST /api/orchestrator/tasks/{id}/discuss
// composes the one-way orchestrator→chat bridge — the inverse of chat's
// promote-to-task. It fetches the run's task row + transcript through the
// ORCHESTRATOR credential (so the sched-side visibility gates stay
// authoritative: no transcript the caller couldn't already read via
// /logs/{id} can leak into a chat), formats a clamped digest, and creates a
// chat conversation seeded with it via POST /conversations {seed}. Returns
// { conversation_id }; the client navigates to /chat?c=<id>.
//
// The bridge is deliberately one-way and composed here in the BFF: the chat
// server never reads the sched store (ADR-0005 keeps the two databases
// separate), and the orchestrator never writes chat state.

// Transcript budget for the digest: enough for result + recent activity
// without preloading a chat near any model's context edge. Per-message cap
// keeps one giant tool dump from eating the whole budget; the digest keeps
// the TAIL of the transcript (the outcome) when the total overflows.
const DIGEST_TOTAL_CHARS = 24_000;
const DIGEST_PER_MESSAGE_CHARS = 2_000;

type LogMessage = {
  role?: string;
  content?: string;
  tool_name?: string;
  is_error?: boolean;
};

type LogSession = {
  messages?: LogMessage[];
  cost?: number;
};

type TaskRow = {
  id: string;
  prompt?: string;
  name?: string;
  status?: string;
  error_message?: string | null;
  result?: string | null;
  completed_at?: string | null;
  recurrence?: string | null;
  artifacts?: Array<{ name?: string; path?: string }> | null;
};

function clip(text: string, max: number): string {
  if (text.length <= max) return text;
  return `${text.slice(0, max)}\n[…truncated…]`;
}

function formatDigest(task: TaskRow, session: LogSession): string {
  const lines: string[] = [];
  lines.push(`You are looking at the transcript of a finished scheduled task run. The user wants to discuss it — answer questions about what happened using the transcript below.`);
  lines.push("");
  lines.push(`## Task`);
  lines.push(`- ID: ${task.id}`);
  if (task.name) lines.push(`- Name: ${task.name}`);
  lines.push(`- Status: ${task.status ?? "unknown"}`);
  if (task.recurrence) lines.push(`- Recurrence: ${task.recurrence}`);
  if (task.completed_at) lines.push(`- Completed: ${task.completed_at}`);
  if (typeof session.cost === "number") lines.push(`- Cost: $${session.cost.toFixed(4)}`);
  lines.push(`- Prompt: ${clip(task.prompt ?? "", DIGEST_PER_MESSAGE_CHARS)}`);
  if (task.error_message) lines.push(`- Error: ${clip(task.error_message, DIGEST_PER_MESSAGE_CHARS)}`);
  if (task.result) {
    lines.push("");
    lines.push(`## Result`);
    lines.push(clip(task.result, DIGEST_PER_MESSAGE_CHARS * 2));
  }
  const artifacts = (task.artifacts ?? []).filter(Boolean);
  if (artifacts.length > 0) {
    lines.push("");
    lines.push(`## Artifacts`);
    for (const a of artifacts.slice(0, 20)) {
      lines.push(`- ${a.name ?? a.path ?? "unnamed"}`);
    }
  }

  lines.push("");
  lines.push(`## Transcript`);
  const rendered = (session.messages ?? []).map((m) => {
    const who = m.tool_name ? `tool:${m.tool_name}${m.is_error ? " (error)" : ""}` : (m.role ?? "unknown");
    return `[${who}] ${clip(m.content ?? "", DIGEST_PER_MESSAGE_CHARS)}`;
  });
  // Keep the TAIL when over budget: the end of a run (the outcome) matters
  // more to a discussion than its first tool calls.
  const kept: string[] = [];
  let used = 0;
  for (let i = rendered.length - 1; i >= 0; i--) {
    used += rendered[i].length + 1;
    if (used > DIGEST_TOTAL_CHARS) {
      kept.push(`[…${i + 1} earlier transcript message(s) omitted…]`);
      break;
    }
    kept.push(rendered[i]);
  }
  kept.reverse();
  lines.push(...(kept.length > 0 ? kept : ["(empty transcript)"]));
  return lines.join("\n");
}

export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const auth = await resolveOrchestratorAuth(request);
  if (!auth) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { taskId } = await context.params;
  const encoded = encodeURIComponent(taskId);

  // Both reads go through the caller's orchestrator credential — a task or
  // transcript the caller can't read via the normal endpoints 403/404s here
  // identically.
  let taskRes: Response;
  let logRes: Response;
  try {
    [taskRes, logRes] = await Promise.all([
      orchestratorFetch(auth, `/tasks/${encoded}`, { method: "GET" }),
      orchestratorFetch(auth, `/logs/${encoded}`, { method: "GET" }),
    ]);
  } catch (err) {
    return NextResponse.json(
      { error: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }
  if (!taskRes.ok) {
    return NextResponse.json({ error: "task not found" }, { status: taskRes.status });
  }
  if (!logRes.ok) {
    return NextResponse.json({ error: "no transcript for this task" }, { status: logRes.status });
  }
  const task = (await taskRes.json()) as TaskRow;
  const log = (await logRes.json()) as LogSession;

  const promptPreview = (task.name || task.prompt || task.id).trim().slice(0, 60);
  const { upstream, error } = await chatServerProxy(session, "/conversations", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      title: `Discuss run: ${promptPreview}`,
      seed: formatDigest(task, log),
    }),
  });
  if (error) return error;
  if (!upstream.ok) {
    const text = await upstream.text();
    return new NextResponse(text, { status: upstream.status });
  }
  const conv = (await upstream.json()) as { ID?: string; id?: string };
  return NextResponse.json({ conversation_id: conv.id ?? conv.ID ?? null });
}
