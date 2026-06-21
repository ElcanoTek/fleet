import { describe, expect, it } from "vitest";
import { NextRequest } from "next/server";
import { verifyOrigin } from "./csrf";

function req(opts: {
  url?: string;
  origin?: string | null;
  host?: string | null;
  forwardedHost?: string | null;
}): NextRequest {
  const headers = new Headers();
  if (opts.origin !== null && opts.origin !== undefined) headers.set("origin", opts.origin);
  if (opts.host !== null && opts.host !== undefined) headers.set("host", opts.host);
  if (opts.forwardedHost !== null && opts.forwardedHost !== undefined) {
    headers.set("x-forwarded-host", opts.forwardedHost);
  }
  return new NextRequest(opts.url ?? "http://chat.example.com/api/foo", {
    method: "POST",
    headers,
  });
}

describe("verifyOrigin", () => {
  it("accepts matching same-origin request", () => {
    const r = req({
      url: "http://chat.example.com/api/foo",
      origin: "http://chat.example.com",
      host: "chat.example.com",
    });
    expect(verifyOrigin(r).ok).toBe(true);
  });

  it("rejects missing Origin header", () => {
    const r = req({ url: "http://chat.example.com/api/foo", host: "chat.example.com" });
    const result = verifyOrigin(r);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.response.status).toBe(403);
  });

  it("rejects cross-origin POST", () => {
    const r = req({
      url: "http://chat.example.com/api/foo",
      origin: "http://evil.example.net",
      host: "chat.example.com",
    });
    const result = verifyOrigin(r);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.response.status).toBe(403);
  });

  it("rejects malformed Origin header", () => {
    const r = req({
      url: "http://chat.example.com/api/foo",
      origin: "not-a-url",
      host: "chat.example.com",
    });
    const result = verifyOrigin(r);
    expect(result.ok).toBe(false);
  });

  it("honors x-forwarded-host ahead of host (reverse proxy case)", () => {
    const r = req({
      url: "http://127.0.0.1:3000/api/foo",
      origin: "https://chat.example.com",
      host: "127.0.0.1:3000",
      forwardedHost: "chat.example.com",
    });
    expect(verifyOrigin(r).ok).toBe(true);
  });

  it("rejects mismatching origin even when host matches the URL", () => {
    // Classic attack scenario: attacker's page submits a form to our
    // host. The browser sets Origin to the attacker's page, the Host
    // header is still ours. Must reject.
    const r = req({
      url: "https://chat.example.com/api/auth/login",
      origin: "https://attacker.example",
      host: "chat.example.com",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("accepts port-specific same-origin (localhost dev)", () => {
    const r = req({
      url: "http://localhost:3000/api/foo",
      origin: "http://localhost:3000",
      host: "localhost:3000",
    });
    expect(verifyOrigin(r).ok).toBe(true);
  });
});
