// The one-card approval read a running card polls for its progress and
// outcome (docs/APPROVAL-PROGRESS.md): session-gated, and forwarded to the
// chat server's one-card GET with the ids encoded.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerProxyMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/csrf", () => ({ verifyOrigin: () => ({ ok: true }) }));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerProxy: (...args: unknown[]) => chatServerProxyMock(...args),
}));

import { GET } from "./route";

const context = { params: Promise.resolve({ conversationId: "conv-1", approvalId: "ap-1" }) };

describe("GET /api/conversations/[conversationId]/approvals/[approvalId]", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerProxyMock.mockReset();
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
  });

  it("reads the one card from the chat server and passes it through", async () => {
    const body = { pending_approvals: [], resolved_approvals: [{ approval_id: "ap-1", executing: true }] };
    chatServerProxyMock.mockResolvedValue({ upstream: new Response(JSON.stringify(body), { status: 200 }) });
    const res = await GET(new NextRequest("https://fleet.example.com/api/conversations/conv-1/approvals/ap-1"), context);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(body);
    const [, path, init] = chatServerProxyMock.mock.calls[0];
    expect(path).toBe("/conversations/conv-1?approval_id=ap-1");
    expect(init).toMatchObject({ method: "GET" });
  });

  it("refuses an anonymous read without calling upstream", async () => {
    getServerSessionMock.mockResolvedValueOnce(null);
    const res = await GET(new NextRequest("https://fleet.example.com/api/conversations/conv-1/approvals/ap-1"), context);
    expect(res.status).toBe(401);
    expect(chatServerProxyMock).not.toHaveBeenCalled();
  });
});
