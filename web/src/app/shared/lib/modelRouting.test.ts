import { describe, expect, it } from "vitest";
import { catalogModelRoutes, modelIsAvailable, type ModelProvider } from "./modelRouting";

const direct: ModelProvider = { name: "direct", type: "openai", models: ["gpt-4o"], catch_all: false };
const router: ModelProvider = { name: "router", type: "openrouter", models: [], catch_all: true };

describe("provider-aware model choices", () => {
  it("rejects the screenshot's default and other OpenRouter slugs on a native-only workspace", () => {
    expect(modelIsAvailable("google/gemini-3.8-flash", [direct], true)).toBe(false);
    expect(modelIsAvailable("openai/gpt-4o", [direct], true)).toBe(false);
    expect(modelIsAvailable("direct/gpt-4o", [direct], true)).toBe(true);
    expect(modelIsAvailable("gpt-4o", [direct], true)).toBe(true);
  });

  it("does not advertise OpenRouter models through a native catch-all", () => {
    const native = { ...direct, models: [], catch_all: true };
    expect(modelIsAvailable("google/gemini-3.8-flash", [native, router], true)).toBe(false);
    expect(modelIsAvailable("direct/custom-model", [native], true)).toBe(true);
    // A typed provider-local identifier remains supported by execution.
    expect(modelIsAvailable("custom-model", [native])).toBe(true);
    expect(modelIsAvailable("google/gemini-3.8-flash", [router, native], true)).toBe(true);
  });

  it("preserves listed custom gateway routes and OpenRouter compatibility", () => {
    const gateway = { ...direct, models: ["google/gemini-3.8-flash"] };
    expect(modelIsAvailable("google/gemini-3.8-flash", [gateway], true)).toBe(true);
    expect(modelIsAvailable("google/gemini-3.8-flash", [direct, router], true)).toBe(true);
    expect(modelIsAvailable("google/gemini-3.8-flash", [], true)).toBe(false);
    expect(modelIsAvailable("google/gemini-3.8-flash", null, true)).toBe(true);
  });

  it("offers explicit routes through each shadowed OpenRouter catch-all", () => {
    const native = { ...direct, models: [], catch_all: true };
    const slug = "google/gemini-3.8-flash";
    expect(catalogModelRoutes(slug, [native])).toEqual([]);
    expect(catalogModelRoutes(slug, [native, router, { ...router, name: "backup" }])).toEqual([
      { slug: `router/${slug}`, provider: "router" },
      { slug: `backup/${slug}`, provider: "backup" },
    ]);
    expect(catalogModelRoutes(slug, [router, native])).toEqual([{ slug }]);
  });
});
