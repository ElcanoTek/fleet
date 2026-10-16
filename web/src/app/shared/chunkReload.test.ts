import { afterEach, describe, expect, it, vi } from "vitest";
import { AUTO_RELOAD_WINDOW_MS, claimChunkAutoReload, isChunkLoadError } from "./chunkReload";

describe("isChunkLoadError", () => {
  it("recognizes a failed chunk or dynamic import in every shape it arrives in", () => {
    const named = new Error("Failed to load chunk /_next/static/chunks/abc.js from runtime");
    named.name = "ChunkLoadError";
    expect(isChunkLoadError(named)).toBe(true);
    for (const message of [
      "Failed to load chunk /_next/static/chunks/abc.js",
      "Loading chunk 123 failed.",
      "Loading CSS chunk app-layout failed",
      "Failed to fetch dynamically imported module: https://x/_next/static/chunks/a.js",
      "Importing a module script failed.",
      "error loading dynamically imported module: https://x/a.js",
    ]) {
      expect(isChunkLoadError(new TypeError(message)), message).toBe(true);
    }
  });

  it("leaves ordinary errors alone", () => {
    expect(isChunkLoadError(new TypeError("Cannot read properties of undefined (reading 'map')"))).toBe(false);
    expect(isChunkLoadError(new Error("Load failed"))).toBe(false);
    expect(isChunkLoadError("Loading chunk 1 failed")).toBe(false);
    expect(isChunkLoadError(null)).toBe(false);
  });
});

describe("claimChunkAutoReload", () => {
  afterEach(() => {
    window.sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it("allows one automatic reload per window, then another after it", () => {
    const t0 = 1_000_000;
    expect(claimChunkAutoReload(t0)).toBe(true);
    expect(claimChunkAutoReload(t0 + 1_000)).toBe(false);
    expect(claimChunkAutoReload(t0 + AUTO_RELOAD_WINDOW_MS - 1)).toBe(false);
    expect(claimChunkAutoReload(t0 + AUTO_RELOAD_WINDOW_MS)).toBe(true);
  });

  it("refuses when session storage is unavailable, since nothing could stop a loop", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("denied", "SecurityError");
    });
    expect(claimChunkAutoReload()).toBe(false);
  });
});
