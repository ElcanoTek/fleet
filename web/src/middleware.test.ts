import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

// Middleware is the ONE request-time gate for the unified frontend. It must:
// redirect unauthenticated page loads to /login, 401 unauthenticated /api
// calls, let public paths through, bounce already-authenticated users away
// from /login, gate BOTH views (/chat/* and /orchestrator/*), and accept BOTH
// login paths (elcano_auth / password cookie via getSessionFromRequest, AND
// moc's username/password Bearer token).

const getSessionFromRequestMock = vi.fn();
const getRedirectUrlMock = vi.fn(
  (_req: unknown, pathname: string) => new URL(`https://chat.elcanotek.com${pathname}`),
);

vi.mock("@/app/lib/auth", () => ({
  getSessionFromRequest: (...args: unknown[]) => getSessionFromRequestMock(...args),
  getRedirectUrl: (...args: unknown[]) => getRedirectUrlMock(...(args as [unknown, string])),
}));
vi.mock("@/app/lib/buildId", () => ({
  BUILD_ID_HEADER: "x-build-id",
  currentBuildId: () => "test-build",
}));

import { middleware } from "../middleware";

function req(path: string, headers?: Record<string, string>) {
  return new NextRequest(`https://chat.elcanotek.com${path}`, { headers });
}

describe("middleware", () => {
  beforeEach(() => {
    getSessionFromRequestMock.mockReset();
    getRedirectUrlMock.mockClear();
  });

  it("redirects an unauthenticated page request to /login", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("401s an unauthenticated /api request (no redirect loop)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/api/conversations"));
    expect(res.status).toBe(401);
  });

  it("lets an authenticated request through (either cookie)", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "elcano" });
    const res = await middleware(req("/"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("bounces an already-authenticated user away from /login to /chat", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "password" });
    const res = await middleware(req("/login"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/chat");
  });

  it("lets the public elcano-login route through without a session", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/api/auth/elcano-login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("serves /login to an unauthenticated user", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  // ── Widened matcher gates BOTH views ────────────────────────────────────
  it("gates /chat/* with the SAME session check", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/chat"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("gates /orchestrator/* with the SAME session check (no separate gate)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/orchestrator"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("admits an elcano_auth session to /orchestrator without re-login", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "elcano" });
    const res = await middleware(req("/orchestrator"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  // ── BOTH login paths resolve ────────────────────────────────────────────
  it("admits a request carrying a moc Bearer token (no cookie)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/orchestrator", { authorization: "Bearer moc-token-123" }));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("admits an orchestrator API request carrying a moc Bearer token", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(
      req("/api/orchestrator/stats", { authorization: "Bearer moc-token-123" }),
    );
    expect(res.status).toBe(200);
  });

  it("lets the public moc orchestrator login route through without a session", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await middleware(req("/api/orchestrator/auth/login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });
});
