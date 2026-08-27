import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch, chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/shared-files — proxy to chat-server's shared file library (the
 * cross-chat files admins stage once and every conversation's agent can
 * read). GET lists the library for any member; POST (admin, enforced
 * upstream) uploads one or more files. Error bodies (400 bad name/folder,
 * 403, 409 duplicate, 413 over a cap) pass through verbatim so the page can
 * show the server's own words.
 */

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  return chatServerPassthrough(session, "/shared-files", { method: "GET" });
}

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
  // (Same shape as /api/attachments.)
  const headers = new Headers();
  headers.set("Content-Type", contentType);

  let upstream: Response;
  try {
    upstream = await chatServerFetch(session, "/shared-files", {
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
