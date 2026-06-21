// POST /api/bug-report
//
// Forwards a frustrated user's chat conversation to the Jules API
// (https://jules.googleapis.com/v1alpha) so Jules can read the transcript,
// figure out what went wrong, and open a pull request against the dev
// branch of this repo with a fix.
//
// JULES_API_KEY is required server-side. Source repo and target branch are
// overridable via JULES_SOURCE and JULES_BRANCH so different deploys can
// point at different forks. JULES_API_BASE lets tests stand up a mock.
//
// User-facing error messages do NOT mention Jules — the integration is an
// internal implementation detail and the customer-facing UX presents this
// as "report a bug to our team". Operators can still see the underlying
// HTTP detail in the response's `detail` field.

import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";
import { currentBuildId } from "@/app/lib/buildId";
import { buildJulesPrompt, getJulesConfig, type ExportEnvelope } from "@/app/lib/jules";

export const runtime = "nodejs";

type BugReportBody = {
  conversationId?: string;
  note?: string;
};

// Outbound timeout to Jules. Jules' /sessions endpoint normally returns in
// well under a second; anything beyond 30s is a connectivity/upstream
// problem and we shouldn't make the user's browser sit on a hung TCP
// socket. Fail closed and tell the user to retry.
const JULES_FETCH_TIMEOUT_MS = 30_000;

// Hard cap on the optional reporter note. Keeps a paste-bomb from eating
// the entire prompt budget before any transcript turns are even
// considered. 4 KB is plenty for a few sentences of context.
const MAX_NOTE_BYTES = 4_000;

export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  let body: BugReportBody;
  try {
    body = (await request.json()) as BugReportBody;
  } catch {
    return NextResponse.json({ error: "invalid JSON body" }, { status: 400 });
  }

  const conversationId = (body.conversationId ?? "").trim();
  if (!conversationId) {
    return NextResponse.json({ error: "conversationId is required" }, { status: 400 });
  }

  const { apiKey, baseUrl, source, branch } = getJulesConfig();
  if (!apiKey) {
    return NextResponse.json(
      { error: "bug reporting isn't configured on this server" },
      { status: 503 },
    );
  }

  const exportResponse = await chatServerFetch(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/export`,
    { method: "GET" },
  );
  if (!exportResponse.ok) {
    const detail = await safeText(exportResponse);
    return NextResponse.json(
      { error: `couldn't load conversation (${exportResponse.status})`, detail },
      { status: exportResponse.status === 404 ? 404 : 502 },
    );
  }

  let envelope: ExportEnvelope;
  try {
    envelope = (await exportResponse.json()) as ExportEnvelope;
  } catch {
    return NextResponse.json({ error: "conversation export was not valid JSON" }, { status: 502 });
  }

  if (!Array.isArray(envelope.history) || envelope.history.length === 0) {
    return NextResponse.json(
      { error: "this conversation has no messages yet" },
      { status: 400 },
    );
  }

  // Lockdown chats are explicitly allowed: bug reporting requires the
  // user to opt in by clicking "Report bug with this chat", and an
  // operator who has wired up bug reporting is signaling that they want
  // users in every chat type to be able to flag problems. The original
  // "lockdown means nothing leaves" framing was about background sync,
  // not user-initiated actions.

  const origin = request.headers.get("origin") ?? request.nextUrl.origin;
  const note = typeof body.note === "string" ? body.note.slice(0, MAX_NOTE_BYTES) : undefined;
  const { prompt, title, truncated, messageCount } = buildJulesPrompt({
    userEmail: session.email,
    note,
    exportEnvelope: envelope,
    appUrl: origin,
    buildId: currentBuildId(),
  });

  const julesPayload = {
    prompt,
    title,
    sourceContext: {
      source,
      githubRepoContext: { startingBranch: branch },
    },
    automationMode: "AUTO_CREATE_PR",
    requirePlanApproval: false,
  };

  let julesResp: Response;
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), JULES_FETCH_TIMEOUT_MS);
  try {
    julesResp = await fetch(`${baseUrl}/sessions`, {
      method: "POST",
      headers: {
        "X-Goog-Api-Key": apiKey,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(julesPayload),
      signal: controller.signal,
    });
  } catch (err) {
    const aborted = controller.signal.aborted;
    return NextResponse.json(
      {
        error: aborted
          ? "the bug-report service took too long — please retry"
          : "couldn't reach the bug-report service",
        detail: String(err),
      },
      { status: 502 },
    );
  } finally {
    clearTimeout(timeoutId);
  }

  const julesText = await safeText(julesResp);
  if (!julesResp.ok) {
    return NextResponse.json(
      { error: `bug-report service returned ${julesResp.status}`, detail: julesText },
      { status: 502 },
    );
  }

  let julesJson: Record<string, unknown> = {};
  try {
    julesJson = julesText ? (JSON.parse(julesText) as Record<string, unknown>) : {};
  } catch {
    // If Jules returned non-JSON we can still tell the client we submitted —
    // the URL hint will just be missing.
  }

  const sessionName = typeof julesJson.name === "string" ? julesJson.name : null;
  const sessionId = typeof julesJson.id === "string" ? julesJson.id : extractIdFromName(sessionName);
  const sessionUrl =
    typeof julesJson.url === "string"
      ? julesJson.url
      : sessionId
        ? `https://jules.google.com/session/${sessionId}`
        : null;

  return NextResponse.json({
    ok: true,
    sessionName,
    sessionId,
    sessionUrl,
    truncated,
    messageCount,
    branch,
    source,
  });
}

async function safeText(response: Response): Promise<string> {
  try {
    return await response.text();
  } catch {
    return "";
  }
}

function extractIdFromName(name: string | null): string | null {
  if (!name) return null;
  const parts = name.split("/");
  return parts[parts.length - 1] || null;
}
