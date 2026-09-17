import { beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest } from "next/server";
import { GET } from "./route";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { validateSlug } from "@/app/lib/openrouterModels";

vi.mock("@/app/lib/auth", () => ({ getServerSession: vi.fn() }));
vi.mock("@/app/lib/chatServer", () => ({ chatServerProxy: vi.fn() }));
vi.mock("@/app/lib/openrouterModels", () => ({
  MODELS_PAGE_URL: "https://openrouter.ai/models", validateSlug: vi.fn(),
}));

describe("model-check routing", () => {
  beforeEach(() => { vi.resetAllMocks(); });

  async function check(route: Record<string, unknown>) {
    vi.mocked(getServerSession).mockResolvedValue({ email: "user@example.com", exp: 9999999999, source: "password" });
    vi.mocked(chatServerProxy).mockResolvedValue({ upstream: Response.json(route) });
    return GET(new NextRequest("http://localhost/api/model-check?slug=direct%2Fgpt-4o"));
  }

  it("returns routing failures even if the model exists in the public catalog", async () => {
    const res = await check({ allowed: false, reason: "provider_unavailable", message: "Choose a workspace model" });
    expect(await res.json()).toMatchObject({ allowed: false, reason: "provider_unavailable", models_url: "/settings/admin/providers" });
    expect(validateSlug).not.toHaveBeenCalled();
  });

  it("does not validate direct-provider identifiers against OpenRouter", async () => {
    expect(await (await check({ allowed: true, provider_type: "openai" })).json()).toMatchObject({ allowed: true });
    expect(validateSlug).not.toHaveBeenCalled();
    expect(chatServerProxy).toHaveBeenCalledWith(expect.objectContaining({ email: "user@example.com" }), "/llm-provider-models?slug=direct%2Fgpt-4o", { method: "GET" });
  });

  it("retains catalog checks for OpenRouter", async () => {
    vi.mocked(validateSlug).mockResolvedValue({ ok: true });
    expect(await (await check({ allowed: true, provider_type: "openrouter" })).json()).toMatchObject({ allowed: true });
    expect(validateSlug).toHaveBeenCalledWith("direct/gpt-4o");
  });

  it("requires an authenticated session", async () => {
    vi.mocked(getServerSession).mockResolvedValue(null);
    const res = await GET(new NextRequest("http://localhost/api/model-check?slug=anything"));
    expect(res.status).toBe(401);
    expect(chatServerProxy).not.toHaveBeenCalled();
  });
});
