import { afterEach, describe, expect, it, vi } from "vitest";
import {
  loadCatwalkProviders,
  parseCatwalkProviders,
  __resetCatwalkCacheForTests,
} from "./catwalkModels";

// The catwalk catalog (catwalk.charm.sh) is the no-auth model database the
// /api/catwalk-models proxy serves so pickers can expand catch-all workspace
// providers. These tests cover the payload normalisation and the module
// cache; the network is always mocked.

const SAMPLE = [
  {
    id: "anthropic",
    name: "Anthropic",
    type: "anthropic",
    models: [
      {
        id: "claude-opus-4-8",
        name: "Claude Opus 4.8",
        context_window: 1000000,
        cost_per_1m_out: 25,
      },
      { id: "", name: "dropped — no id" },
    ],
  },
  { id: "", name: "dropped — no provider id" },
  { id: "bare", models: "not-an-array" },
];

describe("parseCatwalkProviders", () => {
  it("normalises providers and drops id-less entries", () => {
    const out = parseCatwalkProviders(SAMPLE);
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({
      id: "anthropic",
      name: "Anthropic",
      type: "anthropic",
    });
    expect(out[0].models).toEqual([
      {
        id: "claude-opus-4-8",
        name: "Claude Opus 4.8",
        contextWindow: 1000000,
        costPer1MOut: 25,
      },
    ]);
    expect(out[1]).toMatchObject({ id: "bare", name: "bare", models: [] });
  });

  it("throws on a non-array or empty payload", () => {
    expect(() => parseCatwalkProviders({ providers: [] })).toThrow(/not an array/);
    expect(() => parseCatwalkProviders([])).toThrow(/no usable entries/);
  });
});

describe("loadCatwalkProviders", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    __resetCatwalkCacheForTests();
  });

  it("fetches once and serves the cache afterwards", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => SAMPLE });
    vi.stubGlobal("fetch", fetchMock);
    const first = await loadCatwalkProviders();
    const second = await loadCatwalkProviders();
    expect(first).toBe(second);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("propagates an upstream failure (the route maps it to 502)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 503 }));
    await expect(loadCatwalkProviders()).rejects.toThrow(/503/);
  });
});
