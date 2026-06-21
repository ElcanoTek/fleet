import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/server-config — proxies chat-server's /server-config (lockdown
// capability flag + allow-list) and merges in Next.js-side capability
// flags the Go server doesn't know about. `julesEnabled` is one of those:
// the JULES_API_KEY lives in the Next.js env, so the chat UI asks here
// whether the bug-report → Jules affordance should render.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const upstream = await chatServerFetch(session.email, "/server-config", { method: "GET" });
  const text = await upstream.text();

  const julesEnabled = Boolean(process.env.JULES_API_KEY);

  if (!upstream.ok) {
    return new NextResponse(text, {
      status: upstream.status,
      headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
    });
  }

  let merged: Record<string, unknown>;
  try {
    const parsed = text ? (JSON.parse(text) as unknown) : {};
    merged =
      parsed && typeof parsed === "object" && !Array.isArray(parsed)
        ? { ...(parsed as Record<string, unknown>), julesEnabled }
        : { julesEnabled };
  } catch {
    merged = { julesEnabled };
  }
  return NextResponse.json(merged, { status: upstream.status });
}
