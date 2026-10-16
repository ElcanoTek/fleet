import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  __resetDeployDriftForTests,
  PROBE_INTERVAL_MS,
  probeDeploy,
  startDeployDriftProbe,
  useUpdateAvailable,
} from "./deployDrift";

// The one deploy probe per tab: it flips to "update available" only when the
// server reports a different, real build id, never in a dev build, never on a
// failed probe, and a burst of resume events costs one request.

function versionResponse(body: unknown, init: ResponseInit = { status: 200 }) {
  return new Response(JSON.stringify(body), { ...init, headers: { "Content-Type": "application/json" } });
}

function setVisibility(state: DocumentVisibilityState) {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
}

describe("deployDrift", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    __resetDeployDriftForTests();
    vi.stubEnv("NEXT_PUBLIC_BUILD_ID", "client-build");
    fetchMock = vi.fn(async () => versionResponse({ buildId: "client-build" }));
    vi.stubGlobal("fetch", fetchMock);
    setVisibility("visible");
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  it("reports an update when the server runs a different build", async () => {
    fetchMock.mockResolvedValue(versionResponse({ buildId: "server-build" }));
    const { result } = renderHook(() => useUpdateAvailable());
    expect(result.current).toBe(false);
    await act(() => probeDeploy());
    expect(result.current).toBe(true);
    expect(fetchMock).toHaveBeenCalledWith("/api/version", { cache: "no-store" });
  });

  it("stays quiet when the builds match, the probe fails, or the body has no build id", async () => {
    const { result } = renderHook(() => useUpdateAvailable());
    await act(() => probeDeploy());
    fetchMock.mockResolvedValueOnce(versionResponse({ error: "Unauthorized" }, { status: 401 }));
    await act(() => probeDeploy());
    fetchMock.mockRejectedValueOnce(new TypeError("Load failed"));
    await act(() => probeDeploy());
    // The mocked e2e lane answers /api/version with this shape.
    fetchMock.mockResolvedValueOnce(versionResponse({ build_id: "test" }));
    await act(() => probeDeploy());
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(result.current).toBe(false);
  });

  it("never probes a dev build", async () => {
    vi.stubEnv("NEXT_PUBLIC_BUILD_ID", undefined);
    await probeDeploy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("shares one request between overlapping probes and stops once drift is known", async () => {
    let resolve!: (r: Response) => void;
    fetchMock.mockImplementationOnce(() => new Promise<Response>((r) => (resolve = r)));
    const first = probeDeploy();
    const second = probeDeploy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    resolve(versionResponse({ buildId: "server-build" }));
    await Promise.all([first, second]);
    await probeDeploy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("probes on start, on coming back into view, and on the interval, but not while hidden", async () => {
    vi.useFakeTimers();
    const stop = startDeployDriftProbe();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    setVisibility("hidden");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    setVisibility("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(3);

    stop();
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(PROBE_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});
