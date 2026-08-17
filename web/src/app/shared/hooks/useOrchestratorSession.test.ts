import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor, act } from "@testing-library/react";
import { useOrchestratorSession } from "./useOrchestratorSession";

// Pins the initial /me probe's failure taxonomy: only 401/403 are auth
// verdicts. A 5xx — which the orchestrator answers when its fail-closed
// session-epoch lookup can't reach the chat DB — or a thrown network failure
// says nothing about the session, so it must surface as `unreachable`
// instead of flipping to the login card mid-incident.
// Mirrors the chat plane's bootstrapFailure contract.

function meResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function probeWith(response: () => Promise<Response>) {
  vi.stubGlobal("fetch", vi.fn(response));
  return renderHook(() => useOrchestratorSession());
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.localStorage.clear();
});

describe("useOrchestratorSession initial probe", () => {
  it("surfaces a 500 as unreachable — not signed-out", async () => {
    window.localStorage.setItem("orchestratorToken", "bearer-1");
    const { result } = probeWith(async () => meResponse(500, { detail: "Session check failed" }));
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(true);
    expect(result.current.signedIn).toBe(false);
    expect(result.current.noAccess).toBe(false);
    // Leftover moc bearer is purged on load (#1115); auth is the cookie now.
    expect(window.localStorage.getItem("orchestratorToken")).toBeNull();
  });

  it("surfaces a thrown network failure as unreachable", async () => {
    const { result } = probeWith(async () => {
      throw new TypeError("fetch failed");
    });
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(true);
    expect(result.current.signedIn).toBe(false);
  });

  it("keeps 401 as signed-out and purges leftover bearer keys", async () => {
    window.localStorage.setItem("orchestratorToken", "stale");
    window.localStorage.setItem("userToken", "legacy");
    const { result } = probeWith(async () => meResponse(401, { detail: "Unauthorized" }));
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(false);
    expect(result.current.signedIn).toBe(false);
    expect(window.localStorage.getItem("orchestratorToken")).toBeNull();
    expect(window.localStorage.getItem("userToken")).toBeNull();
  });

  it("keeps 403 as the no-access verdict", async () => {
    const { result } = probeWith(async () => meResponse(403, { detail: "not_a_member" }));
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.noAccess).toBe(true);
    expect(result.current.unreachable).toBe(false);
    expect(result.current.signedIn).toBe(false);
  });

  it("signs in on an authenticated /me", async () => {
    const { result } = probeWith(async () =>
      meResponse(200, { authenticated: true, username: "ops", role: "admin" }),
    );
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.signedIn).toBe(true);
    expect(result.current.username).toBe("ops");
    expect(result.current.unreachable).toBe(false);
  });

  it("retires username/password login without writing a token", async () => {
    const { result } = probeWith(async () =>
      meResponse(200, { authenticated: true, username: "ops", role: "admin" }),
    );
    await waitFor(() => expect(result.current.ready).toBe(true));
    await act(async () => {
      await expect(result.current.login("ops", "secret")).resolves.toBe(false);
    });
    expect(window.localStorage.getItem("orchestratorToken")).toBeNull();
    expect(window.localStorage.getItem("userToken")).toBeNull();
    await waitFor(() => expect(result.current.error).toMatch(/retired/i));
  });
});
