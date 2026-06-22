// Verifies the Next.js client-config proxy: auth gate (401 without a session)
// and verbatim passthrough of the chat-server's /client-config JSON.

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

const CONFIG = {
  branding: {
    app_name: "Fleet",
    login_title: "Welcome aboard.",
    login_tagline: "Sign in to your workspace and pick up where you left off.",
    share_title: "Fleet — your team's AI workspace",
    share_description: "…",
  },
  empty_state: { cards: [], protocol_pills: [] },
};

describe("GET /api/client-config", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerFetchMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0 });
    chatServerFetchMock.mockResolvedValue(
      new Response(JSON.stringify(CONFIG), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("proxies the chat-server config verbatim for a member", async () => {
    const res = await GET();
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.branding.app_name).toBe("Fleet");
    expect(body.empty_state.cards).toEqual([]);
    // Forwards the member's email to the (member-gated) upstream.
    expect(chatServerFetchMock).toHaveBeenCalledWith("alice@example.com", "/client-config", {
      method: "GET",
    });
  });

  it("returns 401 when there is no session", async () => {
    getServerSessionMock.mockResolvedValue(null);
    const res = await GET();
    expect(res.status).toBe(401);
    expect(chatServerFetchMock).not.toHaveBeenCalled();
  });

  it("forwards upstream errors verbatim", async () => {
    chatServerFetchMock.mockResolvedValue(new Response("not_a_member", { status: 403 }));
    const res = await GET();
    expect(res.status).toBe(403);
    expect(await res.text()).toBe("not_a_member");
  });
});
