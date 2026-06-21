import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { chatServerFetch, getChatServerBase, getSharedToken, chatServerHeaders } from "./chatServer";

describe("chatServer.ts", () => {
  const originalEnv = process.env;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    process.env = { ...originalEnv };
    fetchMock = vi.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
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
      const headers = chatServerHeaders("user@example.com");
      expect(headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(headers.get("X-User-Email")).toBe("user@example.com");
    });

    it("preserves extra headers", () => {
      process.env.CHAT_SERVER_TOKEN = "test-token";
      const headers = chatServerHeaders("user@example.com", { "X-Custom": "custom-value" });
      expect(headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(headers.get("X-User-Email")).toBe("user@example.com");
      expect(headers.get("X-Custom")).toBe("custom-value");
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
      await chatServerFetch("user@example.com", "/api/test");

      expect(fetchMock).toHaveBeenCalledTimes(1);
      const [url, init] = fetchMock.mock.calls[0];

      expect(url).toBe("http://chat.example.com/api/test");
      expect(init.cache).toBe("no-store");
      expect(init.headers.get("X-Chat-Server-Token")).toBe("test-token");
      expect(init.headers.get("X-User-Email")).toBe("user@example.com");
    });

    it("sets Content-Type to application/json if body is provided", async () => {
      await chatServerFetch("user@example.com", "/api/test", { body: JSON.stringify({ a: 1 }) });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.headers.get("Content-Type")).toBe("application/json");
    });

    it("does not override Content-Type if already provided", async () => {
      await chatServerFetch("user@example.com", "/api/test", {
        body: "custom body",
        headers: { "Content-Type": "text/plain" },
      });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.headers.get("Content-Type")).toBe("text/plain");
    });

    it("passes through extra RequestInit options", async () => {
      await chatServerFetch("user@example.com", "/api/test", { method: "POST" });

      const [, init] = fetchMock.mock.calls[0];
      expect(init.method).toBe("POST");
    });

    it("throws if token is missing", async () => {
      delete process.env.CHAT_SERVER_TOKEN;
      await expect(chatServerFetch("user@example.com", "/api/test")).rejects.toThrow("Missing required environment variable: CHAT_SERVER_TOKEN");
    });
  });
});
