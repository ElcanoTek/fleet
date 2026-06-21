import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";
import { MODELS_PAGE_URL, validateSlug } from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

/**
 * POST /api/chat
 *
 * Thin proxy around chat-server's POST /chat. We verify the session cookie,
 * forward the request to chat-server with the shared-secret + user-email
 * headers, and pipe the SSE body straight back to the browser.
 *
 * The request body matches chat-server's contract:
 *   { conversation_id?, message, persona?, model?, title?, enabled_optional? }
 *
 * The response is an SSE stream with event types:
 *   conversation, reasoning.start/delta/end, text.delta, tool.call,
 *   tool.result, turn.completed, turn.error
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const bodyText = await request.text();

  // Enforce the completion-price ceiling before we proxy. The client also
  // validates, but we re-check here so direct API calls can't bypass it.
  const modelError = await guardModel(bodyText);
  if (modelError) return modelError;

  let upstream: Response;
  try {
    upstream = await chatServerFetch(session.email, "/chat", {
      method: "POST",
      body: bodyText,
      signal: request.signal,
    });
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text().catch(() => upstream.statusText);
    return new NextResponse(text, { status: upstream.status });
  }

  return new Response(upstream.body, {
    status: 200,
    headers: {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}

async function guardModel(bodyText: string): Promise<NextResponse | null> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
    // Malformed JSON — let chat-server return its own 400.
    return null;
  }
  const slug =
    parsed && typeof parsed === "object" && "model" in parsed
      ? (parsed as { model?: unknown }).model
      : undefined;
  if (typeof slug !== "string" || !slug.trim()) return null;

  try {
    const result = await validateSlug(slug);
    if (result.ok || result.reason !== "over_budget") return null;
    return NextResponse.json(
      { error: result.message, models_url: result.modelsUrl },
      { status: 400 },
    );
  } catch {
    // Catalog fetch failed — fail open rather than block the user on an
    // upstream OpenRouter outage.
    return null;
  }
}

// Re-exported so other routes / tests can reference the same link without
// hardcoding it in multiple places.
export { MODELS_PAGE_URL };
