import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The manifest now reads the deployment's splash color from the client-config
// bundle by fetching the token-gated /theme.css server-side, so these tests stub
// fetch. The default case must keep working when that call fails — a missing
// splash color is cosmetic and must never break manifest generation.
const originalFetch = globalThis.fetch;

beforeEach(() => {
  process.env.CHAT_SERVER_URL = "http://127.0.0.1:8080";
  process.env.CHAT_SERVER_TOKEN = "test-token";
});

afterEach(() => {
  delete process.env.NEXT_PUBLIC_APP_NAME;
  globalThis.fetch = originalFetch;
  vi.resetModules();
});

function stubTheme(css: string, ok = true) {
  globalThis.fetch = vi.fn(async () => ({ ok, text: async () => css })) as unknown as typeof fetch;
}

describe("web app manifest", () => {
  it("uses the neutral Fleet name and complete install icon set by default", async () => {
    delete process.env.NEXT_PUBLIC_APP_NAME;
    stubTheme("", false);
    const { default: manifest } = await import("./manifest");

    const value = await manifest();
    expect(value.name).toBe("Fleet");
    expect(value.short_name).toBe("Fleet");
    expect(value.icons).toEqual([
      expect.objectContaining({ src: "/app-icons/icon-192.png", purpose: "any" }),
      expect.objectContaining({ src: "/app-icons/icon-512.png", purpose: "any" }),
      expect.objectContaining({ src: "/app-icons/maskable-icon-512.png", purpose: "maskable" }),
    ]);
  });

  it("uses the deployment's white-label app name", async () => {
    process.env.NEXT_PUBLIC_APP_NAME = "Example Workspace";
    stubTheme("", false);
    const { default: manifest } = await import("./manifest");

    await expect(manifest()).resolves.toEqual(
      expect.objectContaining({ name: "Example Workspace", short_name: "Example Workspace" }),
    );
  });

  it("takes the splash color from the bundle's dark --color-bg", async () => {
    stubTheme('html:root[data-theme="dark"]{--color-primary:#FFDF03;--color-bg:#0C0A09;}');
    const { default: manifest } = await import("./manifest");

    const value = await manifest();
    expect(value.background_color).toBe("#0C0A09");
    expect(value.theme_color).toBe("#0C0A09");
  });

  it("ignores the light block, since a manifest has only one splash color", async () => {
    stubTheme(
      'html:root[data-theme="light"]{--color-bg:#FAFAF9;}html:root[data-theme="dark"]{--color-bg:#0C0A09;}',
    );
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#0C0A09");
  });

  it("falls back to the built-in default when the bundle declares no colors", async () => {
    stubTheme("/* fleet brand theme (client-config bundle) */");
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#1a0b1e");
  });

  it("rejects a value that is not a color fleet would emit", async () => {
    // Defense in depth: the upstream validates too, but this value goes to the
    // OS rather than the DOM, so a surprise here should degrade, not propagate.
    stubTheme('html:root[data-theme="dark"]{--color-bg:url(javascript:alert(1));}');
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#1a0b1e");
  });

  it("falls back when the theme fetch throws", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new Error("backend down");
    }) as unknown as typeof fetch;
    const { default: manifest } = await import("./manifest");

    expect((await manifest()).background_color).toBe("#1a0b1e");
  });
});
