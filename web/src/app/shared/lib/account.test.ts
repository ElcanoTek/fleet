import { afterEach, describe, expect, it, vi } from "vitest";
import { ME_TTL_MS, fetchMe, invalidateMe } from "./account";

const ME = { email: "a@example.com", role: "member", team_id: "quant", admin: false };

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function stubFetch(impl: () => Promise<Response>) {
  const fn = vi.fn(impl);
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("fetchMe", () => {
  it("reads /api/me once for concurrent callers", async () => {
    const fetchMock = stubFetch(async () => Response.json(ME));
    const [a, b] = await Promise.all([fetchMe(), fetchMe()]);
    expect(a).toEqual(ME);
    expect(b).toEqual(ME);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith("/api/me", { cache: "no-store" });
  });

  it("reuses an answer inside the TTL and re-reads after it", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const fetchMock = stubFetch(async () => Response.json(ME));
    await fetchMe();
    await fetchMe();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    vi.setSystemTime(Date.now() + ME_TTL_MS + 1);
    await fetchMe();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("re-reads on fresh and after invalidateMe", async () => {
    const fetchMock = stubFetch(async () => Response.json(ME));
    await fetchMe();
    await fetchMe({ fresh: true });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    invalidateMe();
    await fetchMe();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("answers null without caching on a failure", async () => {
    const fetchMock = stubFetch(async () => new Response("down", { status: 500 }));
    expect(await fetchMe()).toBeNull();
    fetchMock.mockImplementation(async () => {
      throw new TypeError("Failed to fetch");
    });
    expect(await fetchMe()).toBeNull();
    fetchMock.mockImplementation(async () => Response.json(ME));
    expect(await fetchMe()).toEqual(ME);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("normalises missing fields", async () => {
    stubFetch(async () => Response.json({ email: "a@example.com" }));
    expect(await fetchMe()).toEqual({ email: "a@example.com", role: "", team_id: "", admin: false });
  });

  it("does not let a read in flight across invalidateMe repopulate the cache", async () => {
    let release!: (r: Response) => void;
    const fetchMock = stubFetch(
      () =>
        new Promise<Response>((res) => {
          release = res;
        }),
    );
    const stale = fetchMe();
    invalidateMe();
    release(Response.json({ ...ME, team_id: "old" }));
    expect((await stale)?.team_id).toBe("old");
    fetchMock.mockImplementation(async () => Response.json(ME));
    expect((await fetchMe())?.team_id).toBe("quant");
  });
});
