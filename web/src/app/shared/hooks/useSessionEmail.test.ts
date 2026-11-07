import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { setBootstrapForTest } from "@/app/lib/bootstrap";
import { useSessionEmail } from "./useSessionEmail";

afterEach(() => {
  setBootstrapForTest(null);
  vi.unstubAllGlobals();
});

describe("useSessionEmail", () => {
  it("uses the bootstrap's email without a request", () => {
    setBootstrapForTest({ session: { email: "boot@example.com" } });
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useSessionEmail());
    expect(result.current).toBe("boot@example.com");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("asks /api/session once when the bootstrap has no session", async () => {
    setBootstrapForTest({ session: null });
    const fetchMock = vi.fn(async () => new Response(JSON.stringify({ email: "api@example.com" })));
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useSessionEmail());
    expect(result.current).toBe("");
    await waitFor(() => expect(result.current).toBe("api@example.com"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledWith("/api/session", { cache: "no-store" });
  });

  it("stays empty on a failed read", async () => {
    setBootstrapForTest({ session: null });
    const fetchMock = vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    });
    vi.stubGlobal("fetch", fetchMock);
    const { result } = renderHook(() => useSessionEmail());
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(result.current).toBe("");
  });
});
