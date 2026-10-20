// Stop a running approved call (docs/APPROVAL-STOP.md): CSRF- and
// session-gated, and forwarded to the chat server's stop route with the ids
// encoded.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerProxyMock = vi.fn();
const verifyOriginMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/csrf", () => ({ verifyOrigin: (...args: unknown[]) => verifyOriginMock(...args) }));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerProxy: (...args: unknown[]) => chatServerProxyMock(...args),
}));

import { POST } from "./route";

const context = { params: Promise.resolve({ conversationId: "conv-1", approvalId: "ap-1" }) };
const req = () => new NextRequest("https://fleet.example.com/api/conversations/conv-1/approvals/ap-1/stop", { method: "POST" });

describe("POST /api/conversations/[conversationId]/approvals/[approvalId]/stop", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerProxyMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
  });

  it("forwards the stop and passes the card's state through", async () => {
    const body = { status: "approved", executing: true, stopping: true, stoppable: true };
    chatServerProxyMock.mockResolvedValue({ upstream: new Response(JSON.stringify(body), { status: 200 }) });
    const res = await POST(req(), context);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(body);
    const [, path, init] = chatServerProxyMock.mock.calls[0];
    expect(path).toBe("/conversations/conv-1/approvals/ap-1/stop");
    expect(init).toMatchObject({ method: "POST" });
  });

  it("refuses an anonymous stop without calling upstream", async () => {
    getServerSessionMock.mockResolvedValueOnce(null);
    const res = await POST(req(), context);
    expect(res.status).toBe(401);
    expect(chatServerProxyMock).not.toHaveBeenCalled();
  });

  it("refuses a cross-origin stop without calling upstream", async () => {
    verifyOriginMock.mockReturnValueOnce({ ok: false, response: new Response("forbidden", { status: 403 }) });
    const res = await POST(req(), context);
    expect(res.status).toBe(403);
    expect(chatServerProxyMock).not.toHaveBeenCalled();
  });
});
