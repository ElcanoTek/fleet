import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { useOrchestratorSession } from "./useOrchestratorSession";

// Pins the initial /me probe's failure taxonomy: only 401/403 are auth
// verdicts. A 5xx — which the orchestrator answers when its fail-closed
// session-epoch lookup can't reach the chat DB — or a thrown network failure
// says nothing about the session, so it must surface as `unreachable` (and
// keep the stored bearer) instead of flipping to the login card mid-incident.
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
  it("surfaces a 500 as unreachable — not signed-out — and keeps the bearer", async () => {
    window.localStorage.setItem("orchestratorToken", "bearer-1");
    const { result } = probeWith(async () => meResponse(500, { detail: "Session check failed" }));
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(true);
    expect(result.current.signedIn).toBe(false);
    expect(result.current.noAccess).toBe(false);
    // The token is NOT self-healed away: the backend said nothing about it.
    expect(window.localStorage.getItem("orchestratorToken")).toBe("bearer-1");
  });

  it("surfaces a thrown network failure as unreachable", async () => {
    const { result } = probeWith(async () => {
      throw new TypeError("fetch failed");
    });
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(true);
    expect(result.current.signedIn).toBe(false);
  });

  it("keeps 401 as signed-out and self-heals the stale bearer", async () => {
    window.localStorage.setItem("orchestratorToken", "stale");
    const { result } = probeWith(async () => meResponse(401, { detail: "Unauthorized" }));
    await waitFor(() => expect(result.current.ready).toBe(true));
    expect(result.current.unreachable).toBe(false);
    expect(result.current.signedIn).toBe(false);
    expect(window.localStorage.getItem("orchestratorToken")).toBeNull();
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
});
