// The delete flow's first production click 404'd: the orchestrator route
// (#1174) shipped without this per-route proxy file, and there is no
// catch-all to fall back on. This test pins the proxy's existence AND the
// exact upstream path, so dropping either breaks visibly.

import { describe, expect, it, vi } from "vitest";
import { NextRequest, NextResponse } from "next/server";

const proxyMock = vi.fn(async (..._args: unknown[]) => NextResponse.json({ deleted: true }));

vi.mock("../../../_lib/proxy", () => ({
  proxyToOrchestrator: (request: unknown, path: unknown) => proxyMock(request, path),
}));

import { DELETE } from "./route";

describe("DELETE /api/orchestrator/tasks/[taskId]/permanent", () => {
  it("proxies to the orchestrator's /tasks/{id}/permanent with the id encoded", async () => {
    const request = new NextRequest("http://localhost/api/orchestrator/tasks/abc%2F1/permanent", {
      method: "DELETE",
    });
    const res = await DELETE(request, { params: Promise.resolve({ taskId: "abc/1" }) });
    expect(res.status).toBe(200);
    expect(proxyMock).toHaveBeenCalledWith(request, "/tasks/abc%2F1/permanent");
  });
});
