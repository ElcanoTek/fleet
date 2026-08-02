import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { NextRequest } from "next/server";

// Logout clears BOTH session cookies (elcano_session + the shared elcano_auth)
// and returns the user to chat's own /login — not the auth service. Deletions
// are RAW appended Set-Cookie headers (a name-keyed cookie store would
// collapse the two elcano_auth variants into one), so the tests read
// getSetCookie() rather than mocking next/headers.

import { POST } from "./route";

function postReq(origin: string | null) {
  const headers: Record<string, string> = {};
  if (origin) headers["origin"] = origin;
  return new NextRequest("https://chat.elcanotek.com/api/auth/logout", { method: "POST", headers });
}

function cleared(res: Response, name: string): string[] {
  return res.headers
    .getSetCookie()
    .filter((c) => c.startsWith(`${name}=;`) && /Max-Age=0/i.test(c));
}

describe("POST /api/auth/logout", () => {
  const originalEnv = process.env;

  beforeEach(() => {
    process.env = { ...originalEnv };
  });

  afterEach(() => {
    process.env = originalEnv;
  });

  it("clears both cookies and redirects to chat's /login", async () => {
    process.env.AUTH_COOKIE_DOMAIN = "elcanotek.com";
    const res = await POST(postReq("https://chat.elcanotek.com"));

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login");

    // chat's own HMAC cookie, host-only (no domain).
    const session = cleared(res, "elcano_session");
    expect(session).toHaveLength(1);
    expect(session[0]).toMatch(/HttpOnly/i);
    expect(session[0]).not.toMatch(/Domain=/i);

    // The shared cookie is deleted in BOTH shapes: the configured parent
    // domain AND host-only. AUTH_COOKIE_DOMAIN can drift from how the auth
    // service actually minted the cookie, and a deletion that misses the
    // cookie's shape silently no-ops — the user lands on /login and the next
    // load signs them straight back in.
    const elcano = cleared(res, "elcano_auth");
    expect(elcano).toHaveLength(2);
    expect(elcano.filter((c) => /Domain=elcanotek\.com/i.test(c))).toHaveLength(1);
    expect(elcano.filter((c) => !/Domain=/i.test(c))).toHaveLength(1);
  });

  it("sends only the host-only elcano_auth deletion when AUTH_COOKIE_DOMAIN is unset (dev)", async () => {
    delete process.env.AUTH_COOKIE_DOMAIN;
    const res = await POST(postReq("https://chat.elcanotek.com"));

    const elcano = cleared(res, "elcano_auth");
    expect(elcano).toHaveLength(1);
    expect(elcano[0]).not.toMatch(/Domain=/i);
  });

  it("marks deletions Secure on https so they match Secure-minted cookies", async () => {
    const res = await POST(postReq("https://chat.elcanotek.com"));
    for (const c of res.headers.getSetCookie()) {
      expect(c).toMatch(/Secure/i);
    }
  });

  it("rejects a cross-origin POST (CSRF) without clearing anything", async () => {
    const res = await POST(postReq("https://evil.example.com"));
    expect(res.status).toBe(403);
    expect(res.headers.getSetCookie()).toHaveLength(0);
  });

  it("rejects a POST with no Origin header", async () => {
    const res = await POST(postReq(null));
    expect(res.status).toBe(403);
    expect(res.headers.getSetCookie()).toHaveLength(0);
  });
});
