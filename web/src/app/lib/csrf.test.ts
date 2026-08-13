import { afterEach, describe, expect, it } from "vitest";
import { NextRequest } from "next/server";
import { verifyOrigin } from "./csrf";

function req(opts: {
  url?: string;
  origin?: string | null;
  host?: string | null;
  forwardedHost?: string | null;
}): NextRequest {
  const headers = new Headers();
  if (opts.origin !== null && opts.origin !== undefined)
    headers.set("origin", opts.origin);
  if (opts.host !== null && opts.host !== undefined)
    headers.set("host", opts.host);
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
    const r = req({
      url: "http://chat.example.com/api/foo",
      host: "chat.example.com",
    });
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

// When the deployment's canonical origin is configured, it — not the
// client-supplied x-forwarded-host — is the host the Origin header must match.
// Same trust rule as lib/auth.ts (getRedirectUrl/isSecureRequest).
describe("verifyOrigin — configured canonical origin", () => {
  afterEach(() => {
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
  });

  it("rejects an Origin that only matches a spoofed x-forwarded-host", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    // A request straight to `next start` (bypassing the proxy) where the
    // attacker supplies BOTH the Origin and the x-forwarded-host so they
    // agree with each other — but not with the deployment.
    const r = req({
      url: "https://fleet.example.com/api/foo",
      origin: "https://evil.example.net",
      host: "fleet.example.com",
      forwardedHost: "evil.example.net",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("accepts the canonical Origin even when x-forwarded-host is spoofed", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "https://fleet.example.com/api/foo",
      origin: "https://fleet.example.com",
      host: "fleet.example.com",
      forwardedHost: "evil.example.net",
    });
    expect(verifyOrigin(r).ok).toBe(true);
  });
});

// The loopback exception: a canonical origin is written by bootstrap on every
// deploy, but an operator on a headless box reaches the UI over an SSH tunnel
// — browser at http://localhost:3000 — so their Origin can never equal the
// canonical host. That shape must keep working (it guards every mutating
// route INCLUDING the login form, so rejecting it presents as a total auth
// outage), while a forwarded-header attack from a real remote origin must
// still fail.
describe("verifyOrigin — loopback tunnel with a canonical origin", () => {
  afterEach(() => {
    delete process.env.NEXT_PUBLIC_PUBLIC_ORIGIN;
  });

  it.each(["localhost:3000", "127.0.0.1:3000", "[::1]:3000"])(
    "accepts a loopback Origin matching the connection host (%s)",
    (host) => {
      process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
      const r = req({
        url: `http://${host}/api/auth/login`,
        origin: `http://${host}`,
        host,
      });
      expect(verifyOrigin(r).ok).toBe(true);
    },
  );

  it("rejects a loopback Origin whose port differs from the connection host", () => {
    // Another local web app (e.g. a dev server on :8080) is a different
    // origin and must not be able to CSRF the tunneled UI.
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "http://localhost:3000/api/foo",
      origin: "http://localhost:8080",
      host: "localhost:3000",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("rejects a loopback Origin when the connection host is the real deployment", () => {
    // A victim's browser talking to the deployment sends the deployment's
    // Host — a loopback Origin there is forged traffic, not a tunnel.
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "https://fleet.example.com/api/foo",
      origin: "http://localhost:3000",
      host: "fleet.example.com",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("ignores x-forwarded-host for the loopback exception", () => {
    // The forwarded header is client-supplied: agreeing with the Origin must
    // not open the loopback branch when the connection host is not loopback.
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "https://fleet.example.com/api/foo",
      origin: "http://localhost:3000",
      host: "fleet.example.com",
      forwardedHost: "localhost:3000",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("rejects a lookalike loopback subdomain", () => {
    // `localhost.evil.example` resolves wherever the attacker wants; only the
    // exact loopback shapes qualify.
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "http://localhost.evil.example:3000/api/foo",
      origin: "http://localhost.evil.example:3000",
      host: "localhost.evil.example:3000",
    });
    expect(verifyOrigin(r).ok).toBe(false);
  });

  it("still rejects a non-loopback cross-origin POST", () => {
    process.env.NEXT_PUBLIC_PUBLIC_ORIGIN = "https://fleet.example.com";
    const r = req({
      url: "https://fleet.example.com/api/foo",
      origin: "https://evil.example.net",
      host: "fleet.example.com",
    });
    const result = verifyOrigin(r);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.response.status).toBe(403);
  });
});
