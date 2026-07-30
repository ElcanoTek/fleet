import { readFileSync } from "node:fs";
import { join } from "node:path";

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  BRANDING_DEFAULTS,
  DEFAULT_BACKGROUND_DARK,
  DEFAULT_BACKGROUND_LIGHT,
  __resetBrandingCache,
  getServerBranding,
} from "./serverBranding";

const APP_DIR = join(import.meta.dirname ?? __dirname, "..");

const PAYLOAD = {
  app_name: "Reklaim",
  login_title: "Reklaim what's yours.",
  login_tagline: "Sign in and pick up where you left off.",
  share_title: "Reklaim — AI workspace",
  share_description: "Persistent conversations with real tool use.",
  background_light: "#FAFAF9",
  background_dark: "#0A0908",
};

describe("getServerBranding", () => {
  const originalEnv = process.env;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    process.env = { ...originalEnv };
    process.env.CHAT_SERVER_URL = "http://chat.example.com";
    process.env.CHAT_SERVER_TOKEN = "test-token";
    delete process.env.NEXT_PUBLIC_APP_NAME;
    fetchMock = vi.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
    __resetBrandingCache();
  });

  afterEach(() => {
    process.env = originalEnv;
    vi.restoreAllMocks();
    __resetBrandingCache();
  });

  function ok(body: unknown): Response {
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  }

  it("resolves the bundle's identity", async () => {
    fetchMock.mockResolvedValue(ok(PAYLOAD));
    const b = await getServerBranding();
    expect(b.appName).toBe("Reklaim");
    expect(b.loginTitle).toBe("Reklaim what's yours.");
    expect(b.loginTagline).toBe("Sign in and pick up where you left off.");
    expect(b.shareTitle).toBe("Reklaim — AI workspace");
    expect(b.shareDescription).toBe("Persistent conversations with real tool use.");
    expect(b.backgroundLight).toBe("#FAFAF9");
    expect(b.backgroundDark).toBe("#0A0908");
  });

  it("sends the shared token and never a user identity", async () => {
    fetchMock.mockResolvedValue(ok(PAYLOAD));
    await getServerBranding();
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://chat.example.com/brand/meta");
    expect((init.headers as Record<string, string>)["X-Chat-Server-Token"]).toBe("test-token");
    // Identity-less by design — the login page has no session to forward.
    expect(JSON.stringify(init.headers)).not.toContain("X-User-Email");
  });

  it("carries a timeout so a hung backend cannot block a page render", async () => {
    fetchMock.mockResolvedValue(ok(PAYLOAD));
    await getServerBranding();
    const [, init] = fetchMock.mock.calls[0];
    expect(init.signal).toBeInstanceOf(AbortSignal);
  });

  // Every failure path is cosmetic: branding must never fail a render.
  it.each([
    ["backend unreachable", () => fetchMock.mockRejectedValue(new Error("ECONNREFUSED"))],
    ["non-2xx", () => fetchMock.mockResolvedValue(new Response("nope", { status: 503 }))],
    ["unparseable JSON", () => fetchMock.mockResolvedValue(new Response("<html>", { status: 200 }))],
    ["JSON null", () => fetchMock.mockResolvedValue(ok(null))],
    ["JSON array", () => fetchMock.mockResolvedValue(ok([1, 2, 3]))],
    ["timeout", () => fetchMock.mockRejectedValue(new DOMException("aborted", "TimeoutError"))],
  ])("falls back to defaults on %s", async (_name, arrange) => {
    arrange();
    const b = await getServerBranding();
    expect(b.appName).toBe("Fleet");
    expect(b.loginTitle).toBe(BRANDING_DEFAULTS.loginTitle);
    expect(b.backgroundDark).toBeNull();
    expect(b.backgroundLight).toBeNull();
  });

  it("fills per-field defaults for a partial payload", async () => {
    fetchMock.mockResolvedValue(ok({ app_name: "Acme" }));
    const b = await getServerBranding();
    expect(b.appName).toBe("Acme");
    // share_title mirrors the backend's own default-to-app_name rule.
    expect(b.shareTitle).toBe("Acme");
    expect(b.loginTitle).toBe(BRANDING_DEFAULTS.loginTitle);
    expect(b.shareDescription).toBe(BRANDING_DEFAULTS.shareDescription);
  });

  it("ignores empty and non-string values rather than rendering blanks", async () => {
    fetchMock.mockResolvedValue(
      ok({
        app_name: "",
        login_title: "   ",
        login_tagline: 42,
        share_description: null,
        background_dark: "",
      }),
    );
    const b = await getServerBranding();
    expect(b.appName).toBe("Fleet");
    expect(b.loginTitle).toBe(BRANDING_DEFAULTS.loginTitle);
    expect(b.loginTagline).toBe(BRANDING_DEFAULTS.loginTagline);
    expect(b.shareDescription).toBe(BRANDING_DEFAULTS.shareDescription);
    expect(b.backgroundDark).toBeNull();
  });

  it("prefers the bundle over NEXT_PUBLIC_APP_NAME — the #894 fix", async () => {
    process.env.NEXT_PUBLIC_APP_NAME = "StaleEnvName";
    fetchMock.mockResolvedValue(ok(PAYLOAD));
    const b = await getServerBranding();
    expect(b.appName).toBe("Reklaim");
  });

  it("still honors NEXT_PUBLIC_APP_NAME when the backend is unreachable", async () => {
    // The env var is no longer the primary source, but it is the only value
    // available if the fetch fails, and an operator who set it should not see
    // "Fleet" in that case.
    vi.resetModules();
    process.env.NEXT_PUBLIC_APP_NAME = "Acme";
    const mod = await import("./serverBranding");
    fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    const b = await mod.getServerBranding();
    expect(b.appName).toBe("Acme");
  });

  it("memoizes within the TTL so a page render is not a round trip", async () => {
    fetchMock.mockResolvedValue(ok(PAYLOAD));
    await getServerBranding();
    await getServerBranding();
    await getServerBranding();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("caches the fallback too, so a down backend does not time out per render", async () => {
    fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
    await getServerBranding();
    await getServerBranding();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("exposes fleet's own backgrounds as the documented fallback pair", () => {
    // These mirror globals.css; consumers use them when the bundle sets none.
    expect(DEFAULT_BACKGROUND_DARK).toBe("#1a0b1e");
    expect(DEFAULT_BACKGROUND_LIGHT).toBe("#f4f6fb");
  });
});

/**
 * The #895 trap, pinned.
 *
 * manifest.ts was written to fetch the bundle palette per request and never did:
 * the route was statically prerendered, so the fetch ran during `next build` in a
 * staging dir with the backend down, and every deployment silently served the
 * fallback. Nothing failed; the only symptom was two live endpoints disagreeing.
 *
 * Any consumer of getServerBranding must therefore be dynamic. layout.tsx gets
 * this implicitly (a root-layout generateMetadata/generateViewport forces dynamic
 * rendering), but a metadata *route* needs the flag spelled out.
 */
describe("branding consumers are request-time, not build-time", () => {
  it("manifest.ts declares force-dynamic", () => {
    const src = readFileSync(join(APP_DIR, "manifest.ts"), "utf8");
    expect(src).toMatch(/export const dynamic\s*=\s*["']force-dynamic["']/);
  });

  it("layout.tsx resolves metadata and viewport per request", () => {
    const src = readFileSync(join(APP_DIR, "layout.tsx"), "utf8");
    // Static `metadata` / `viewport` exports would be evaluated at build time and
    // could not read the bundle at all.
    expect(src).toMatch(/export async function generateMetadata\(/);
    expect(src).toMatch(/export async function generateViewport\(/);
    expect(src).not.toMatch(/export const metadata\s*:/);
    expect(src).not.toMatch(/export const viewport\s*:/);
  });

  it("layout.tsx declares force-dynamic for the whole subtree", () => {
    // generateMetadata alone is NOT enough. Routes that don't otherwise opt out
    // (/settings/*, /no-access, /) get statically prerendered, and Next bakes
    // generateMetadata's output into their HTML at build time — where no backend
    // is reachable. That shipped `<title>Fleet</title>` hardcoded into the
    // artifact for those pages while /login and /chat were correct, so only
    // *some* tabs were wrong. Verified by grepping .next/server/app/*.html.
    const src = readFileSync(join(APP_DIR, "layout.tsx"), "utf8");
    expect(src).toMatch(/export const dynamic\s*=\s*["']force-dynamic["']/);
  });

  it("layout.tsx hardcodes no theme-color literal outside the documented fallback", () => {
    const src = readFileSync(join(APP_DIR, "layout.tsx"), "utf8");
    // The two fleet-purple literals used to live inline here; they now live in
    // serverBranding as named fallbacks so there is one place to keep in sync.
    expect(src).not.toContain('"#1a0b1e"');
    expect(src).not.toContain('"#f4f6fb"');
    expect(src).toContain("DEFAULT_BACKGROUND_DARK");
    expect(src).toContain("DEFAULT_BACKGROUND_LIGHT");
  });
});
