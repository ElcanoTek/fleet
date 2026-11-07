import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { _resetModelTiersForTests, setModelTiers } from "@/app/lib/modelAliases";
import { _resetModelCacheForTests } from "@/app/shared/lib/models";
import { useChatModelCatalog, type ChatModelCatalogOptions } from "./useChatModelCatalog";

// #1960 moved the chat composer's model-catalog state out of chat-experience
// and onto shared/lib/models.ts's cached /api/model-catalog loader. These pin
// the behaviour the move had to keep: one catalog request, pins relabelled,
// the lockdown allow-list, and a failed fetch that leaves the picker usable.

const CATALOG = {
  models: [
    { slug: "acme/open-70b", name: "Acme: Open 70B", context_length: 131072, created: 1700000000, price_prompt: 0.000001, price_completion: 0.000002 },
    { slug: "zeta/other", name: "Zeta: Other", price_completion: 0.000003 },
  ],
};

function catalogFetch(response: () => Promise<unknown>) {
  const fetchMock = vi.fn(async (url: string) => {
    if (String(url).includes("/api/model-catalog")) return response();
    return { ok: false, status: 404, json: async () => ({}) };
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function catalogCalls(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.filter(([url]) => String(url).includes("/api/model-catalog")).length;
}

const OPTIONS: ChatModelCatalogOptions = {
  modelRouting: null,
  workspaceModelTiers: null,
  isLockdown: false,
  lockdownAllowedModels: [],
};

describe("useChatModelCatalog", () => {
  beforeEach(() => {
    _resetModelCacheForTests();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    _resetModelCacheForTests();
    _resetModelTiersForTests();
  });

  it("loads the catalog once, mapped onto RankedModel rows", async () => {
    const fetchMock = catalogFetch(async () => ({ ok: true, status: 200, json: async () => CATALOG }));
    const { result } = renderHook(() => useChatModelCatalog(OPTIONS));
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    expect(catalogCalls(fetchMock)).toBe(1);
    expect(result.current.isLoadingCatalog).toBe(false);
    expect(result.current.catalogModels).toEqual([
      { slug: "acme/open-70b", name: "Acme: Open 70B", contextLength: 131072, created: 1700000000, pricePrompt: 0.000001, priceCompletion: 0.000002 },
      { slug: "zeta/other", name: "Zeta: Other", contextLength: undefined, created: undefined, pricePrompt: undefined, priceCompletion: 0.000003 },
    ]);

    // A second chat mount (or the task form) reuses the shared cache.
    const second = renderHook(() => useChatModelCatalog(OPTIONS));
    await act(async () => {
      await second.result.current.loadCatalogModels();
    });
    expect(catalogCalls(fetchMock)).toBe(1);
    expect(second.result.current.catalogModels).toHaveLength(2);
  });

  it("relabels and tags operator-pinned models in the picker rows", async () => {
    setModelTiers({ pinned: [{ slug: "acme/open-70b", label: "Elcano Nova", tag: "alpha" }] });
    catalogFetch(async () => ({ ok: true, status: 200, json: async () => CATALOG }));
    const { result } = renderHook(() => useChatModelCatalog(OPTIONS));
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    const pinned = result.current.filteredRankedModels.find((m) => m.slug === "acme/open-70b");
    expect(pinned).toMatchObject({ name: "Elcano Nova", pinTag: "alpha", relabelled: true, contextLength: 131072 });

    // A relabelled pin is found by its label, not by the slug it renames.
    act(() => result.current.setModelSearchQuery("nova"));
    expect(result.current.filteredRankedModels.map((m) => m.slug)).toContain("acme/open-70b");
    act(() => result.current.setModelSearchQuery("open-70b"));
    expect(result.current.filteredRankedModels.map((m) => m.slug)).not.toContain("acme/open-70b");
  });

  it("limits a lockdown chat to the allow-list, ignoring the search query", async () => {
    catalogFetch(async () => ({ ok: true, status: 200, json: async () => CATALOG }));
    const { result } = renderHook(() =>
      useChatModelCatalog({
        ...OPTIONS,
        isLockdown: true,
        lockdownAllowedModels: ["zeta/other", "acme/unknown", "zeta/other"],
      }),
    );
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    act(() => result.current.setModelSearchQuery("acme/open-70b"));
    expect(result.current.filteredRankedModels).toEqual([
      expect.objectContaining({ slug: "zeta/other", name: "Zeta: Other" }),
      { slug: "acme/unknown", name: "acme/unknown" },
    ]);
  });

  it("degrades to the tier rows when the catalog fetch fails, without retrying", async () => {
    const fetchMock = catalogFetch(async () => ({ ok: false, status: 502, json: async () => ({}) }));
    const { result } = renderHook(() => useChatModelCatalog(OPTIONS));
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    await waitFor(() => expect(result.current.isLoadingCatalog).toBe(false));
    expect(result.current.catalogModels).toEqual([]);
    // The picker still offers the recommended tiers.
    expect(result.current.filteredRankedModels.length).toBeGreaterThan(0);
    // Once per session regardless of outcome: no request loop on failure.
    await act(async () => {
      await result.current.loadCatalogModels();
    });
    expect(catalogCalls(fetchMock)).toBe(1);
  });

  it("survives a rejected catalog request", async () => {
    catalogFetch(async () => {
      throw new TypeError("network down");
    });
    const { result } = renderHook(() => useChatModelCatalog(OPTIONS));
    await act(async () => {
      await expect(result.current.loadCatalogModels()).resolves.toBeUndefined();
    });
    expect(result.current.catalogModels).toEqual([]);
    expect(result.current.isLoadingCatalog).toBe(false);
  });
});
