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

import { proxy } from "./proxy";

function req(path: string, headers?: Record<string, string>) {
  return new NextRequest(`https://chat.elcanotek.com${path}`, { headers });
}

describe("proxy", () => {
  beforeEach(() => {
    getSessionFromRequestMock.mockReset();
    getRedirectUrlMock.mockClear();
  });

  it("redirects an unauthenticated page request to /login", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("401s an unauthenticated /api request (no redirect loop)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/api/conversations"));
    expect(res.status).toBe(401);
  });

  it("lets an authenticated request through (either cookie)", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "elcano" });
    const res = await proxy(req("/"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("bounces an already-authenticated user away from /login to /chat", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "password" });
    const res = await proxy(req("/login"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/chat");
  });

  it("lets the public elcano-login route through without a session", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/api/auth/elcano-login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("serves /login to an unauthenticated user", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("serves the install manifest without a session", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/manifest.webmanifest"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
    expect(getSessionFromRequestMock).not.toHaveBeenCalled();
  });

  // ── Widened matcher gates BOTH views ────────────────────────────────────
  it("gates /chat/* with the SAME session check", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/chat"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("gates /orchestrator/* with the SAME session check (no separate gate)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/orchestrator"));
    expect(res.status).toBe(307);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");
  });

  it("admits an elcano_auth session to /orchestrator without re-login", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "elcano" });
    const res = await proxy(req("/orchestrator"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  // ── BOTH login paths resolve ────────────────────────────────────────────
  it("admits a request carrying a moc Bearer token (no cookie)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/orchestrator", { authorization: "Bearer moc-token-123" }));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("admits an orchestrator API request carrying a moc Bearer token", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(
      req("/api/orchestrator/stats", { authorization: "Bearer moc-token-123" }),
    );
    expect(res.status).toBe(200);
  });

  it("lets the public moc orchestrator login route through without a session", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/api/orchestrator/auth/login"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  it("lets the pre-auth brand theme stylesheet through without a session (#903)", async () => {
    // layout.tsx links /api/theme on every page, including /login — a 401 here
    // means the login page renders in default colors instead of the bundle's.
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/api/theme"));
    expect(res.status).toBe(200);
    expect(res.headers.get("location")).toBeNull();
  });

  // ── Content-Security-Policy (#590) ──────────────────────────────────────
  // The public /shared view renders assistant-authored HTML in a sandbox=""
  // iframe; sandbox blocks scripts but NOT sub-resource loads, so the page
  // CSP (inherited by the srcdoc iframe) is what stops <img>/@import beacons
  // to attacker hosts. /shared/* must NOT allow external https images.
  it("serves /shared/* without a session and with a restrictive CSP (no external img hosts)", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const res = await proxy(req("/shared/some-token"));
    expect(res.status).toBe(200);
    const csp = res.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("default-src 'self'");
    expect(csp).toContain("img-src 'self' data: blob:");
    expect(csp).not.toContain("https:");
    expect(csp).toContain("connect-src 'self'");
    expect(csp).toContain("style-src 'self' 'unsafe-inline'");
    expect(csp).toContain("object-src 'none'");
    expect(csp).toContain("frame-ancestors 'none'");
  });

  it("carries the baseline CSP on authenticated pages (external https images allowed)", async () => {
    getSessionFromRequestMock.mockResolvedValue({ email: "a@x.com", exp: 0, source: "elcano" });
    const res = await proxy(req("/chat"));
    const csp = res.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("default-src 'self'");
    expect(csp).toContain("img-src 'self' data: blob: https:");
    expect(csp).toContain("frame-ancestors 'none'");
  });

  it("stamps the CSP on redirect and 401 responses too", async () => {
    getSessionFromRequestMock.mockResolvedValue(null);
    const redirect = await proxy(req("/chat"));
    expect(redirect.headers.get("content-security-policy")).toContain("default-src 'self'");
    const denied = await proxy(req("/api/conversations"));
    expect(denied.headers.get("content-security-policy")).toContain("default-src 'self'");
  });
});
