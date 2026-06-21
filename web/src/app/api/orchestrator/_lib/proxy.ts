import { NextRequest, NextResponse } from "next/server";
import { verifyOrigin } from "@/app/lib/csrf";
import { orchestratorFetch } from "@/app/lib/mocServer";
import { resolveOrchestratorAuth } from "./auth";

// Methods that mutate state get a CSRF Origin check before proxying, mirroring
// chat's proxy convention.
const MUTATING = new Set(["POST", "PUT", "PATCH", "DELETE"]);

type ProxyOptions = {
  // Whether to forward the raw request body (default: for mutating methods).
  forwardBody?: boolean;
  // Override the upstream method (rarely needed).
  method?: string;
};

/**
 * proxyToOrchestrator is the one funnel every /api/orchestrator/* route uses.
 * It resolves the caller's credential (moc bearer OR elcano cookie), enforces
 * CSRF on mutating verbs, forwards the request to the orchestrator at :8000,
 * and pipes the upstream status + body straight back to the browser.
 *
 * `upstreamPath` is the orchestrator-side path (e.g. "/stats", "/tasks?limit=20").
 */
export async function proxyToOrchestrator(
  request: NextRequest,
  upstreamPath: string,
  options: ProxyOptions = {},
): Promise<NextResponse> {
  const method = options.method ?? request.method;

  if (MUTATING.has(method)) {
    const csrf = verifyOrigin(request);
    if (!csrf.ok) return csrf.response;
  }

  const auth = await resolveOrchestratorAuth(request);
  if (!auth) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const forwardBody = options.forwardBody ?? MUTATING.has(method);

  const init: RequestInit = { method, signal: request.signal };
  if (forwardBody) {
    // Preserve the incoming Content-Type (JSON vs multipart) and stream the
    // body through. For multipart uploads we forward the raw body + header so
    // the boundary is preserved.
    const contentType = request.headers.get("content-type");
    init.body = await request.arrayBuffer();
    if (contentType) {
      init.headers = { "Content-Type": contentType };
    }
  }

  let upstream: Response;
  try {
    upstream = await orchestratorFetch(auth, upstreamPath, init);
  } catch (err) {
    return NextResponse.json(
      { error: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}

/**
 * Builds the upstream query string from the incoming request's search params,
 * passing through only the named allow-list of params.
 */
export function passThroughQuery(request: NextRequest, allowed: string[]): string {
  const out = new URLSearchParams();
  for (const key of allowed) {
    const v = request.nextUrl.searchParams.get(key);
    if (v !== null && v !== "") out.set(key, v);
  }
  const qs = out.toString();
  return qs ? `?${qs}` : "";
}
