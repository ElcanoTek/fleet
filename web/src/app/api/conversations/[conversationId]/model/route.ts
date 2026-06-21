import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";
import { validateSlug } from "@/app/lib/openrouterModels";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const body = await request.text();

  const modelError = await guardModel(body);
  if (modelError) return modelError;

  const upstream = await chatServerFetch(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/model`,
    { method: "POST", body },
  );
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

async function guardModel(bodyText: string): Promise<NextResponse | null> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(bodyText);
  } catch {
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
    return null;
  }
}
