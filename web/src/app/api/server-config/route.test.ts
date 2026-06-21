// Verifies the Next.js server-config proxy merges in `julesEnabled` from the
// JULES_API_KEY env var. The Go chat-server doesn't know about this flag,
// so the merge has to happen here.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const getServerSessionMock = vi.fn();
const chatServerFetchMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerFetch: (...args: unknown[]) => chatServerFetchMock(...args),
}));

import { GET } from "./route";

describe("GET /api/server-config", () => {
  const originalEnv = process.env;

  beforeEach(() => {
    process.env = { ...originalEnv };
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0 });
    chatServerFetchMock.mockResolvedValue(
      new Response(
        JSON.stringify({
          lockdown_available: true,
          lockdown_only: false,
          lockdown_allowed_models: ["openai/gpt-5"],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
  });

  afterEach(() => {
    process.env = originalEnv;
    vi.restoreAllMocks();
  });

  it("returns julesEnabled=true when JULES_API_KEY is set", async () => {
    process.env.JULES_API_KEY = "k";
    const res = await GET();
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.julesEnabled).toBe(true);
    expect(body.lockdown_available).toBe(true);
  });

  it("returns julesEnabled=false when JULES_API_KEY is unset", async () => {
    delete process.env.JULES_API_KEY;
    const res = await GET();
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.julesEnabled).toBe(false);
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET();
    expect(res.status).toBe(401);
  });

  it("forwards upstream errors verbatim", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("upstream boom", { status: 500 }));
    const res = await GET();
    expect(res.status).toBe(500);
    expect(await res.text()).toBe("upstream boom");
  });

  it("still returns julesEnabled when upstream returns malformed JSON", async () => {
    process.env.JULES_API_KEY = "k";
    chatServerFetchMock.mockResolvedValue(
      new Response("not json", { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const res = await GET();
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.julesEnabled).toBe(true);
  });
});
