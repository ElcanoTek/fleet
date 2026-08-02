import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The manifest resolves the deployment's name and splash color from the
// client-config bundle by fetching the token-gated /brand/meta server-side
// (shared with layout.tsx via lib/serverBranding), so these tests stub fetch.
//
// It previously parsed /theme.css for the color and read the app name from a
// build-time env var. Both moved: one endpoint now serves the whole white-label
// identity, so the manifest and the <meta> tags cannot disagree about what the
// deployment is called or coloured (#894, #895).
//
// Every failure case must still produce a complete manifest — a missing splash
// color or name is cosmetic and must never break manifest generation.
const originalFetch = globalThis.fetch;

beforeEach(() => {
  process.env.CHAT_SERVER_URL = "http://127.0.0.1:8080";
  process.env.CHAT_SERVER_TOKEN = "test-token";
  vi.resetModules();
});

afterEach(() => {
  delete process.env.NEXT_PUBLIC_APP_NAME;
  globalThis.fetch = originalFetch;
  vi.resetModules();
});

/** Stub chat-server's /brand/meta. */
function stubMeta(body: unknown, ok = true) {
  globalThis.fetch = vi.fn(async () => ({
    ok,
    json: async () => body,
  })) as unknown as typeof fetch;
}

describe("web app manifest", () => {
  it("uses the neutral Fleet name and a complete install icon set by default", async () => {
    delete process.env.NEXT_PUBLIC_APP_NAME;
    stubMeta(null, false);
    const { default: manifest } = await import("./manifest");

    const value = await manifest();
    expect(value.name).toBe("Fleet");
    expect(value.short_name).toBe("Fleet");
    // The "any" icon is the bundle's mark via /api/brand/logo — that route
    // redirects to fleet's own mark when no bundle declares one, so an
    // unbranded deployment still gets a valid icon (#895). Maskable stays
    // fleet's padded asset: an arbitrary bundle file has no safe zone and
    // Android would crop it.
    expect(value.icons).toEqual([
      expect.objectContaining({ src: "/api/brand/logo", purpose: "any" }),
      expect.objectContaining({ src: "/app-icons/maskable-icon-512.png", purpose: "maskable" }),
    ]);
  });

  it("takes the app name from the bundle, not the env var", async () => {
    // The #894 fix: branding.app_name wins over NEXT_PUBLIC_APP_NAME.
    process.env.NEXT_PUBLIC_APP_NAME = "StaleEnvName";
    stubMeta({ app_name: "Example Workspace" });
    const { default: manifest } = await import("./manifest");

    await expect(manifest()).resolves.toEqual(
      expect.objectContaining({ name: "Example Workspace", short_name: "Example Workspace" }),
    );
  });

  it("still honors NEXT_PUBLIC_APP_NAME when the backend is unreachable", async () => {
    process.env.NEXT_PUBLIC_APP_NAME = "Example Workspace";
    stubMeta(null, false);
    const { default: manifest } = await import("./manifest");

    await expect(manifest()).resolves.toEqual(
      expect.objectContaining({ name: "Example Workspace" }),
    );
  });

  it("takes the splash color from the bundle's dark background", async () => {
    stubMeta({ app_name: "Reklaim", background_dark: "#0C0A09", background_light: "#FAFAF9" });
    const { default: manifest } = await import("./manifest");

    const value = await manifest();
    expect(value.background_color).toBe("#0C0A09");
    expect(value.theme_color).toBe("#0C0A09");
  });

  it("ignores the light background, since a manifest has only one splash color", async () => {
    stubMeta({ background_light: "#FAFAF9", background_dark: "#0C0A09" });
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#0C0A09");
  });

  it("falls back to the built-in default when the bundle declares no colors", async () => {
    stubMeta({ app_name: "Reklaim" });
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#1a0b1e");
  });

  it("falls back when the meta fetch throws", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new Error("backend down");
    }) as unknown as typeof fetch;
    const { default: manifest } = await import("./manifest");

    const value = await manifest();
    expect(value.background_color).toBe("#1a0b1e");
    expect(value.name).toBe("Fleet");
  });

  // A color the upstream would have rejected can't reach here (validBackground in
  // internal/httpapi/brand_meta.go drops it against the same colorValueRe
  // /theme.css uses), which is why validation lives there and not duplicated
  // here. This asserts the contract holds end to end: an omitted field degrades.
  it("degrades when the backend omits the background rather than emitting empty", async () => {
    stubMeta({ app_name: "Reklaim", background_dark: "" });
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#1a0b1e");
  });
});
