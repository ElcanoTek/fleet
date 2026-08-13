import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { orchestratorFetch, orchestratorHeaders } from "./mocServer";

// The Operations Center is reached with the SAME elcano_session cookie chat is,
// so it forwards the SAME session-epoch claim and honours the same revocation
// verdict. Without that, an admin password reset evicted the stolen cookie from
// chat while /api/orchestrator/* — datasets, task create/rerun, logs, workspace
// files, admin budgets — kept accepting it for the rest of its 14 days.

const cookieDelete = vi.fn();
vi.mock("next/headers", () => ({
  cookies: () => Promise.resolve({ delete: (name: string) => cookieDelete(name) }),
}));

describe("mocServer.ts session epoch", () => {
  const originalEnv = process.env;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    process.env = { ...originalEnv };
    process.env.ORCHESTRATOR_SERVER_URL = "http://moc.example.com";
    process.env.CHAT_SERVER_TOKEN = "test-token";
    fetchMock = vi.fn().mockResolvedValue(new Response("ok"));
    global.fetch = fetchMock as unknown as typeof fetch;
    cookieDelete.mockReset();
  });

  afterEach(() => {
    process.env = originalEnv;
    vi.restoreAllMocks();
  });

  describe("orchestratorHeaders", () => {
    it("forwards the session epoch beside the email", () => {
      const h = orchestratorHeaders({ kind: "cookie", email: "user@example.com", epoch: "abcdef0123456789" });
      expect(h.get("X-User-Email")).toBe("user@example.com");
      expect(h.get("X-User-Session-Epoch")).toBe("abcdef0123456789");
    });

    // An elcano_auth session has no epoch; the orchestrator admits a claimless
    // request rather than locking magic-link users out.
    it("omits the epoch header for a session without one", () => {
      const h = orchestratorHeaders({ kind: "cookie", email: "user@example.com" });
      expect(h.has("X-User-Session-Epoch")).toBe(false);
    });

    // A moc bearer is its own credential and carries no chat session.
    it("sends no epoch on the bearer path", () => {
      const h = orchestratorHeaders({ kind: "bearer", token: "moc-token" });
      expect(h.has("X-User-Session-Epoch")).toBe(false);
      expect(h.get("Authorization")).toBe("Bearer moc-token");
    });
  });

  describe("orchestratorFetch", () => {
    it("forwards the claim upstream", async () => {
      await orchestratorFetch({ kind: "cookie", email: "user@example.com", epoch: "abcdef0123456789" }, "/stats");
      const [url, init] = fetchMock.mock.calls[0];
      expect(url).toBe("http://moc.example.com/stats");
      expect(init.headers.get("X-User-Session-Epoch")).toBe("abcdef0123456789");
    });

    // A revoked session still has a valid signature, so leaving the cookie in
    // place traps the browser on every view it authenticates.
    it("deletes the session cookie when the orchestrator reports a revoked session", async () => {
      fetchMock.mockResolvedValue(
        new Response('{"error":"session_revoked"}', {
          status: 401,
          headers: { "X-Session-Revoked": "1" },
        }),
      );
      const res = await orchestratorFetch({ kind: "cookie", email: "user@example.com", epoch: "stale" }, "/stats");
      expect(cookieDelete).toHaveBeenCalledWith("elcano_session");
      // The body is left untouched so the streaming passthroughs still forward it.
      expect(res.bodyUsed).toBe(false);
    });

    it("leaves the cookie alone for an unrelated 401", async () => {
      fetchMock.mockResolvedValue(new Response("nope", { status: 401 }));
      await orchestratorFetch({ kind: "bearer", token: "expired" }, "/stats");
      expect(cookieDelete).not.toHaveBeenCalled();
    });
  });
});
