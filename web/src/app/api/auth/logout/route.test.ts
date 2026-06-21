import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

// Logout clears BOTH session cookies (elcano_session + the shared elcano_auth)
// and returns the user to chat's own /login — not the auth service. We mock
// next/headers' cookies() to capture what gets set.

const cookieSet = vi.fn();
vi.mock("next/headers", () => ({
  cookies: async () => ({ set: cookieSet }),
}));

import { POST } from "./route";

function postReq(origin: string | null) {
  const headers: Record<string, string> = {};
  if (origin) headers["origin"] = origin;
  return new NextRequest("https://chat.elcanotek.com/api/auth/logout", { method: "POST", headers });
}

function clearedCookie(name: string) {
  return cookieSet.mock.calls.map((c) => c[0]).find((c) => c.name === name);
}

describe("POST /api/auth/logout", () => {
  const originalEnv = process.env;

  beforeEach(() => {
    process.env = { ...originalEnv };
    cookieSet.mockReset();
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
    const session = clearedCookie("elcano_session");
    expect(session).toMatchObject({ value: "", maxAge: 0, httpOnly: true });
    expect(session.domain).toBeUndefined();

    // The shared cookie, deleted on the parent domain so it actually matches.
    const elcano = clearedCookie("elcano_auth");
    expect(elcano).toMatchObject({ value: "", maxAge: 0, domain: "elcanotek.com" });
  });

  it("omits the domain on elcano_auth when AUTH_COOKIE_DOMAIN is unset (host-only / dev)", async () => {
    delete process.env.AUTH_COOKIE_DOMAIN;
    await POST(postReq("https://chat.elcanotek.com"));

    const elcano = clearedCookie("elcano_auth");
    expect(elcano).toMatchObject({ value: "", maxAge: 0 });
    expect(elcano.domain).toBeUndefined();
  });

  it("rejects a cross-origin POST (CSRF) without clearing anything", async () => {
    const res = await POST(postReq("https://evil.example.com"));
    expect(res.status).toBe(403);
    expect(cookieSet).not.toHaveBeenCalled();
  });

  it("rejects a POST with no Origin header", async () => {
    const res = await POST(postReq(null));
    expect(res.status).toBe(403);
    expect(cookieSet).not.toHaveBeenCalled();
  });
});
