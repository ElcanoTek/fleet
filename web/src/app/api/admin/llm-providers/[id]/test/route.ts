import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * POST /api/admin/llm-providers/{id}/test — proxy to chat-server's
 * test-connection probe. Read-only side effects (one authenticated GET against
 * the provider's endpoint, host-side); the response is a key-free summary
 * ({ ok, status, detail, served_model_count, missing_models, latency_ms }).
 */
export async function POST(request: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  const { upstream, error } = await chatServerProxy(
    session,
    `/admin/llm-providers/${encodeURIComponent(id)}/test`,
    { method: "POST" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
