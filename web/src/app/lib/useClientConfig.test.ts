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
  refreshClientConfig,
} from "./useClientConfig";
import {
  _resetModelTiersForTests,
  currentAdvancedModel,
  currentDefaultModel,
} from "./modelAliases";

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
  _resetModelTiersForTests();
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

// The models block (#1187) rides the same payload: it must install the live
// tier pair module-wide and expose it on the hook, and an older server's
// payload (no models field) must leave the compiled-in pair untouched.
describe("useClientConfig shared cache is observable", () => {
  // The Operations Center shell and the task form each mount the hook. When
  // the shell's fetch succeeds and the form's duplicate fetch fails, the form
  // must still receive the tiers — otherwise it never adopts the admin's pair.
  it("propagates one instance's successful fetch to an instance whose own fetch failed", async () => {
    let calls = 0;
    fetchMock.mockImplementation(() => {
      calls += 1;
      if (calls === 1) {
        return Promise.resolve(
          okResponse({
            branding: CLIENT_BRANDING,
            models: { default_model: "acme/frontier-1", advanced_model: "acme/frontier-1-pro" },
          }),
        );
      }
      return Promise.reject(new Error("network"));
    });
    const shell = renderHook(() => useClientConfig());
    const form = renderHook(() => useClientConfig());
    await waitFor(() => expect(shell.result.current.models?.defaultModel).toBe("acme/frontier-1"));
    await waitFor(() => expect(form.result.current.models?.defaultModel).toBe("acme/frontier-1"));
    expect(form.result.current.branding.app_name).toBe("Elcano");
    expect(form.result.current.loading).toBe(false);
  });
});

describe("refreshClientConfig", () => {
  // The chat shell calls this on tab/network return so an admin's tier change
  // reaches an open tab together with the lockdown list it drives.
  it("re-fetches and publishes the new tiers to a mounted instance", async () => {
    fetchMock.mockResolvedValueOnce(
      okResponse({ models: { default_model: "acme/frontier-1", advanced_model: "acme/frontier-1-pro" } }),
    );
    const { result } = renderHook(() => useClientConfig());
    await waitFor(() => expect(result.current.models?.defaultModel).toBe("acme/frontier-1"));

    fetchMock.mockResolvedValueOnce(
      okResponse({ models: { default_model: "acme/frontier-2", advanced_model: "acme/frontier-2-pro" } }),
    );
    await expect(refreshClientConfig()).resolves.toBe(true);
    await waitFor(() => expect(result.current.models?.defaultModel).toBe("acme/frontier-2"));
    expect(currentDefaultModel()).toBe("acme/frontier-2");

    // A failed refresh keeps the last good payload.
    fetchMock.mockRejectedValueOnce(new Error("network"));
    await expect(refreshClientConfig()).resolves.toBe(false);
    expect(result.current.models?.defaultModel).toBe("acme/frontier-2");
  });
});

describe("out-of-order client-config responses", () => {
  // The shell and the task modal both hold the hook and focus/online
  // refreshes overlap, so two fetches can be in flight. An older response
  // landing last would broadcast the obsolete pair and drag a pristine form
  // back to the previous default, for as long as the tab kept refreshing.
  it("ignores a response that a newer one has already overtaken", async () => {
    let releaseOld: (v: unknown) => void = () => {};
    const oldResponse = new Promise((resolve) => {
      releaseOld = resolve;
    });
    fetchMock
      .mockImplementationOnce(() => oldResponse)
      .mockImplementationOnce(() =>
        Promise.resolve(okResponse({ models: { default_model: "acme/new", advanced_model: "acme/new-pro" } })),
      );

    // enabled=false: no mount fetch, so both requests below are ours and the
    // instance still receives whatever any refresh broadcasts.
    const { result } = renderHook(() => useClientConfig(false));
    const stale = refreshClientConfig();
    const fresh = refreshClientConfig();
    await expect(fresh).resolves.toBe(true);
    await waitFor(() => expect(result.current.models?.defaultModel).toBe("acme/new"));

    // The first request now finishes, carrying the superseded pair.
    releaseOld(okResponse({ models: { default_model: "acme/old", advanced_model: "acme/old-pro" } }));
    await expect(stale).resolves.toBe(false);
    expect(result.current.models?.defaultModel).toBe("acme/new");
    expect(currentDefaultModel()).toBe("acme/new");
  });
});

describe("useClientConfig model tiers", () => {
  it("installs the workspace tier pair and returns it", async () => {
    fetchMock.mockResolvedValue(
      okResponse({
        branding: CLIENT_BRANDING,
        models: { default_model: "acme/frontier-1", advanced_model: "acme/frontier-1-pro" },
      }),
    );
    const { result } = renderHook(() => useClientConfig());
    expect(result.current.models).toBeNull();
    await waitFor(() =>
      expect(result.current.models).toEqual({
        defaultModel: "acme/frontier-1",
        advancedModel: "acme/frontier-1-pro",
      }),
    );
    expect(currentDefaultModel()).toBe("acme/frontier-1");
    expect(currentAdvancedModel()).toBe("acme/frontier-1-pro");
  });

  it("keeps the compiled-in pair when an older server sends no models block", async () => {
    const before = { def: currentDefaultModel(), adv: currentAdvancedModel() };
    fetchMock.mockResolvedValue(okResponse({ branding: CLIENT_BRANDING }));
    const { result } = renderHook(() => useClientConfig());
    await waitFor(() => expect(result.current.branding.app_name).toBe("Elcano"));
    expect(result.current.models).toEqual({
      defaultModel: before.def,
      advancedModel: before.adv,
    });
    expect(currentDefaultModel()).toBe(before.def);
  });
});
