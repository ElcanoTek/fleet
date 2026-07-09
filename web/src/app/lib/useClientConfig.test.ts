// Verifies useClientConfig's module-scope cache: crossing between /chat and
// /orchestrator remounts the shell (two routes, one rail), and the remounted
// hook must render the already-fetched branding on its FIRST frame — not
// flash the neutral defaults while /api/client-config re-resolves (the
// "Elcano → Fleet" brand-row flicker).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, act, cleanup, waitFor } from "@testing-library/react";

import {
  useClientConfig,
  DEFAULT_BRANDING,
  __resetClientConfigCacheForTests,
} from "./useClientConfig";

const CLIENT_BRANDING = {
  app_name: "Elcano",
  login_title: "Welcome back.",
  login_tagline: "Chart the course.",
  share_title: "Elcano — AI workspace",
  share_description: "Elcano's workspace.",
};

function okResponse(body: unknown) {
  return {
    ok: true,
    status: 200,
    json: async () => body,
  } as Response;
}

const fetchMock = vi.fn();

beforeEach(() => {
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  fetchMock.mockReset();
  __resetClientConfigCacheForTests();
});

describe("useClientConfig module-scope cache", () => {
  it("starts from neutral defaults on the first mount, then applies the fetch", async () => {
    fetchMock.mockResolvedValue(okResponse({ branding: CLIENT_BRANDING }));

    const { result } = renderHook(() => useClientConfig());
    expect(result.current.branding).toEqual(DEFAULT_BRANDING);
    expect(result.current.loading).toBe(true);

    await waitFor(() => expect(result.current.branding.app_name).toBe("Elcano"));
    expect(result.current.loading).toBe(false);
  });

  it("seeds a remount from the cache — no defaults flash while the re-fetch is in flight", async () => {
    fetchMock.mockResolvedValue(okResponse({ branding: CLIENT_BRANDING }));
    const first = renderHook(() => useClientConfig());
    await waitFor(() => expect(first.result.current.branding.app_name).toBe("Elcano"));
    first.unmount();

    // Second mount's fetch never resolves — the first frame must already be
    // the cached branding, and loading must not report a fresh cold start.
    fetchMock.mockImplementation(() => new Promise(() => {}));
    const second = renderHook(() => useClientConfig());
    expect(second.result.current.branding.app_name).toBe("Elcano");
    expect(second.result.current.loading).toBe(false);
  });

  it("keeps the cached branding when a remount's re-fetch fails", async () => {
    fetchMock.mockResolvedValue(okResponse({ branding: CLIENT_BRANDING }));
    const first = renderHook(() => useClientConfig());
    await waitFor(() => expect(first.result.current.branding.app_name).toBe("Elcano"));
    first.unmount();

    fetchMock.mockRejectedValue(new Error("network down"));
    const second = renderHook(() => useClientConfig());
    // Flush the rejected fetch; the cached branding must survive.
    await act(async () => {
      await Promise.resolve();
    });
    expect(second.result.current.branding.app_name).toBe("Elcano");
  });

  it("falls back to neutral defaults when the first-ever fetch fails", async () => {
    fetchMock.mockRejectedValue(new Error("network down"));
    const { result } = renderHook(() => useClientConfig());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.branding).toEqual(DEFAULT_BRANDING);
  });
});
