// The grouped-approval proxy (docs/GROUPED-APPROVALS.md): CSRF + session
// gating, the upstream path, and a verbatim passthrough of the body and the
// status. Pinned by importing the real route module, so a missing or
// misrouted proxy fails here rather than only in a deployment.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const getServerSessionMock = vi.fn();
const chatServerProxyMock = vi.fn();
const verifyOriginMock = vi.fn();

vi.mock("@/app/lib/auth", () => ({
  getServerSession: (...args: unknown[]) => getServerSessionMock(...args),
}));
vi.mock("@/app/lib/csrf", () => ({
  verifyOrigin: (...args: unknown[]) => verifyOriginMock(...args),
}));
vi.mock("@/app/lib/chatServer", () => ({
  chatServerProxy: (...args: unknown[]) => chatServerProxyMock(...args),
}));

import { POST } from "./route";

const context = { params: Promise.resolve({ conversationId: "conv-1", groupId: "group-1" }) };
const body = JSON.stringify({ approve: ["ap-1"], decline: ["ap-2"] });

function request(): NextRequest {
  return new NextRequest("https://fleet.example.com/api/conversations/conv-1/approval-groups/group-1", {
    method: "POST",
    body,
  });
}

describe("POST /api/conversations/[conversationId]/approval-groups/[groupId]", () => {
  beforeEach(() => {
    getServerSessionMock.mockReset();
    chatServerProxyMock.mockReset();
    verifyOriginMock.mockReset();
    verifyOriginMock.mockReturnValue({ ok: true });
    getServerSessionMock.mockResolvedValue({ email: "alice@example.com", exp: 0, epoch: "e1" });
  });

  it("forwards the decision to the chat server's group endpoint and passes the answer through", async () => {
    chatServerProxyMock.mockResolvedValue({
      upstream: new Response(JSON.stringify({ group_id: "group-1", results: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    });
    const res = await POST(request(), context);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ group_id: "group-1", results: [] });
    const [, path, init] = chatServerProxyMock.mock.calls[0];
    expect(path).toBe("/conversations/conv-1/approval-groups/group-1");
    expect(init).toMatchObject({ method: "POST", body });
  });

  it("passes a refusal through with its status", async () => {
    chatServerProxyMock.mockResolvedValue({
      upstream: new Response("approval \"x\" is not in this approval group", { status: 409 }),
    });
    const res = await POST(request(), context);
    expect(res.status).toBe(409);
    expect(await res.text()).toContain("not in this approval group");
  });

  it("refuses a cross-origin request and an anonymous one without calling upstream", async () => {
    verifyOriginMock.mockReturnValueOnce({ ok: false, response: NextResponse.json({ error: "bad origin" }, { status: 403 }) });
    expect((await POST(request(), context)).status).toBe(403);
    getServerSessionMock.mockResolvedValueOnce(null);
    expect((await POST(request(), context)).status).toBe(401);
    expect(chatServerProxyMock).not.toHaveBeenCalled();
  });
});
