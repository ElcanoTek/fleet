import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * POST /api/attachments
 *
 * Thin proxy around chat-server's POST /attachments. Accepts a multipart
 * body with one or more files under the "files" field, forwards it with
 * the shared-secret headers, and returns the JSON metadata the composer
 * echoes back on the next /api/chat call.
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const contentType = request.headers.get("content-type") ?? "";
  if (!contentType.toLowerCase().startsWith("multipart/form-data")) {
    return NextResponse.json(
      { error: "expected multipart/form-data" },
      { status: 400 },
    );
  }

  // Stream the multipart body straight through — buffering the whole
  // thing in memory would defeat the point of handling large files.
  const headers = new Headers();
  headers.set("Content-Type", contentType);

  let upstream: Response;
  try {
    upstream = await chatServerFetch(session.email, "/attachments", {
      method: "POST",
      headers,
      body: request.body,
      // duplex: "half" is required when streaming a ReadableStream body.
      // @ts-expect-error: Node fetch honors this option; types lag.
      duplex: "half",
      signal: request.signal,
    });
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("content-type") ?? "application/json",
    },
  });
}
