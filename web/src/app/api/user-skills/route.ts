import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// /api/user-skills — the caller's own authored skills (docs/SKILLS.md phase
// 2): GET lists, POST creates.
async function proxy(method: "GET" | "POST", req?: Request) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const init: { method: string; body?: string; headers?: Record<string, string> } = { method };
  if (req && method === "POST") {
    init.body = await req.text();
    init.headers = { "Content-Type": "application/json" };
  }
  const { upstream, error } = await chatServerProxy(session.email, "/user-skills", init);
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text.length > 0 ? text : null, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function GET() {
  return proxy("GET");
}

export async function POST(req: Request) {
  return proxy("POST", req);
}
