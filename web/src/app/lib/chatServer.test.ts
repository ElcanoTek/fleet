import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import {
  chatServerFetch,
  chatServerPassthrough,
  chatServerProxy,
  fetchSessionEpoch,
  getChatServerBase,
  getSharedToken,
  chatServerHeaders,
} from "./chatServer";

// The funnel deletes the session cookie when chat-server reports a revoked
// session, which only a Route Handler may do — so the cookie store is stubbed
// here the way Next.js provides it in that context.
const cookieDelete = vi.fn();
vi.mock("next/headers", () => ({
  cookies: () => Promise.resolve({ delete: (name: string) => cookieDelete(name) }),
}));

describe("chatServer.ts", () => {
  const originalEnv = process.env;
  const user = { email: "user@example.com" };
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    process.env = { ...originalEnv };
    fetchMock = vi.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
    cookieDelete.mockReset();
  });

  afterEach(() => {
    process.env = originalEnv;
    vi.restoreAllMocks();
  });

  describe("getChatServerBase", () => {
    it("returns default base when env var is not set", () => {
      delete process.env.CHAT_SERVER_URL;
      expect(getChatServerBase()).toBe("http://127.0.0.1:8080");
    });

    it("returns env var base when set", () => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com";
      expect(getChatServerBase()).toBe("http://chat.example.com");
    });

    it("strips trailing slashes from env var", () => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com///";
      expect(getChatServerBase()).toBe("http://chat.example.com");
    });
  });

  describe("getSharedToken", () => {
    it("returns token when set", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      expect(getSharedToken()).toBe("test-token");
    });

    it("throws error when token is missing", () => {
      delete process.env.CHAT_SERVER_TOKEN;
      expect(() => getSharedToken()).toThrow("Missing required environment variable: CHAT_SERVER_TOKEN");
    });
  });

  describe("chatServerHeaders", () => {
    it("sets expected headers", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      const headers = chatServerHeaders(user);
      expect(headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(headers.get("X-User-Email")).toBe("user@example.com");
    });

    it("preserves extra headers", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      const headers = chatServerHeaders(user, { "X-Custom": "custom-value" });
      expect(headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(headers.get("X-User-Email")).toBe("user@example.com");
      expect(headers.get("X-Custom")).toBe("custom-value");
    });

    it("forwards the session epoch when the identity carries one", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      const headers = chatServerHeaders({ email: "user@example.com", epoch: "abcdef0123456789" });
      expect(headers.get("X-User-Session-Epoch")).toBe("abcdef0123456789");
    });

    // An elcano_auth session has no epoch to forward; chat-server admits a
    // claimless request rather than locking those users out.
    it("omits the epoch header for an identity without one", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      expect(chatServerHeaders(user).has("X-User-Session-Epoch")).toBe(false);
    });
  });

  describe("chatServerFetch", () => {
    beforeEach(() => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com";
      process.env.CHAT_SERVER_TOKEN = "test-token";
      // mock a successful response
      fetchMock.mockResolvedValue(new Response("ok"));
    });

    it("calls fetch with correct URL and default headers", async () => {
      await chatServerFetch(user, "/api/test");

      expect(fetchMock).toHaveBeenCalledTimes(1);
      const [url, init] = fetchMock.mock.calls[0];

      expect(url).toBe("http://chat.example.com/api/test");
      expect(init.cache).toBe("no-store");
      expect(init.headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(init.headers.get("X-User-Email")).toBe("user@example.com");
    });

    it("sets Content-Type to application/json if body is provided", async () => {
      await chatServerFetch(user, "/api/test", { body: JSON.stringify({ a: 1 }) });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.headers.get("Content-Type")).toBe("application/json");
    });

    it("does not override Content-Type if already provided", async () => {
      await chatServerFetch(user, "/api/test", {
        body: "custom body",
        headers: { "Content-Type": "text/plain" },
      });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.headers.get("Content-Type")).toBe("text/plain");
    });

    it("passes through extra RequestInit options", async () => {
      await chatServerFetch(user, "/api/test", { method: "POST" });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.method).toBe("POST");
    });

    it("throws if token is missing", async () => {
      delete process.env.CHAT_SERVER_TOKEN;
      await expect(chatServerFetch(user, "/api/test")).rejects.toThrow("Missing required environment variable: CHAT_SERVER_TOKEN");
    });

    // A revoked session still has a valid signature, so leaving the cookie in
    // place traps the browser: the request proxy bounces /login back to /chat,
    // which 401s again.
    it("deletes the session cookie when chat-server reports a revoked session", async () => {
      fetchMock.mockResolvedValue(
        new Response('{"error":"session_revoked"}', {
          status: 401,
          headers: { "X-Session-Revoked": "1" },
        }),
      );
      const res = await chatServerFetch({ email: "user@example.com", epoch: "stale" }, "/conversations");
      expect(cookieDelete).toHaveBeenCalledWith("elcano_session");
      // The body is left untouched so streaming callers still forward it.
      expect(res.bodyUsed).toBe(false);
    });

    it("leaves the cookie alone for an unrelated 401", async () => {
      fetchMock.mockResolvedValue(new Response("nope", { status: 401 }));
      await chatServerFetch(user, "/conversations");
      expect(cookieDelete).not.toHaveBeenCalled();
    });
  });

  describe("fetchSessionEpoch", () => {
    beforeEach(() => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com";
      process.env.CHAT_SERVER_TOKEN = "test-token";
    });

    it("reads the epoch chat-server reports for the email", async () => {
      fetchMock.mockResolvedValue(new Response('{"session_epoch":"abcdef0123456789"}', { status: 200 }));
      expect(await fetchSessionEpoch("user@example.com")).toBe("abcdef0123456789");
      const [url, init] = fetchMock.mock.calls[0];
      expect(url).toBe("http://chat.example.com/auth/session-epoch");
      expect(init.headers.get("X-User-Email")).toBe("user@example.com");
    });

    // Null must be distinguishable from an epoch: the mint paths refuse to issue
    // a cookie without one rather than issue a claimless one.
    it("returns null when chat-server cannot answer", async () => {
      fetchMock.mockResolvedValue(new Response("boom", { status: 500 }));
      expect(await fetchSessionEpoch("user@example.com")).toBeNull();

      fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
      expect(await fetchSessionEpoch("user@example.com")).toBeNull();

      fetchMock.mockResolvedValue(new Response("{}", { status: 200 }));
      expect(await fetchSessionEpoch("user@example.com")).toBeNull();
    });
  });

  describe("chatServerProxy", () => {
    beforeEach(() => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com";
      process.env.CHAT_SERVER_TOKEN = "test-token";
    });

    it("returns the raw upstream Response on success (body unread, forwarded verbatim)", async () => {
      const upstream = new Response("payload", { status: 200 });
      fetchMock.mockResolvedValue(upstream);
      const result = await chatServerProxy(user, "/api/test", { method: "GET" });
      expect(result.error).toBeUndefined();
      expect(result.upstream).toBe(upstream);
      expect(result.upstream!.bodyUsed).toBe(false);
    });

    it("forwards a non-2xx upstream as success (not an error)", async () => {
      fetchMock.mockResolvedValue(new Response("nope", { status: 403 }));
      const result = await chatServerProxy(user, "/api/test");
      expect(result.error).toBeUndefined();
      expect(result.upstream!.status).toBe(403);
    });

    it("returns a clean 502 when the fetch rejects (chat-server unreachable)", async () => {
      fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
      const result = await chatServerProxy(user, "/api/test");
      expect(result.upstream).toBeUndefined();
      expect(result.error!.status).toBe(502);
      const body = await result.error!.json();
      expect(body.error).toMatch(/chat-server unreachable: ECONNREFUSED/);
    });
  });

  describe("chatServerPassthrough", () => {
    beforeEach(() => {
      process.env.CHAT_SERVER_URL = "http://chat.example.com";
      process.env.CHAT_SERVER_TOKEN = "test-token";
    });

    // #896: the funnel forwarded only Content-Type, so the filename the Go
    // handler chose never reached the browser and downloads saved as the URL's
    // last path segment.
    it("forwards Content-Disposition from upstream", async () => {
      fetchMock.mockResolvedValue(
        new Response('{"ok":true}', {
          status: 200,
          headers: {
            "Content-Type": "application/json",
            "Content-Disposition": 'attachment; filename="Q3-Planning-a1b2c3d4.json"',
          },
        }),
      );
      const res = await chatServerPassthrough(user, "/projects/x/export");
      expect(res.headers.get("Content-Disposition")).toBe(
        'attachment; filename="Q3-Planning-a1b2c3d4.json"',
      );
      expect(res.status).toBe(200);
      expect(await res.text()).toBe('{"ok":true}');
    });

    it("streams the body through instead of buffering it", async () => {
      // A body that is only readable as a stream would throw if the funnel
      // called .text() on it before constructing the response.
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(new TextEncoder().encode("chunk-1,"));
          controller.enqueue(new TextEncoder().encode("chunk-2"));
          controller.close();
        },
      });
      fetchMock.mockResolvedValue(
        new Response(stream, { status: 200, headers: { "Content-Type": "text/csv" } }),
      );
      const res = await chatServerPassthrough(user, "/export");
      expect(await res.text()).toBe("chunk-1,chunk-2");
      expect(res.headers.get("Content-Type")).toBe("text/csv");
    });

    it("forwards a non-2xx status and its body", async () => {
      fetchMock.mockResolvedValue(new Response('{"error":"forbidden"}', { status: 403 }));
      const res = await chatServerPassthrough(user, "/admin/settings");
      expect(res.status).toBe(403);
      expect(await res.text()).toBe('{"error":"forbidden"}');
    });

    it("returns the 502 shape when chat-server is unreachable", async () => {
      fetchMock.mockRejectedValue(new Error("ECONNREFUSED"));
      const res = await chatServerPassthrough(user, "/admin/settings");
      expect(res.status).toBe(502);
    });
  });
});
