import { test, expect } from "../live/fixtures";

// CANARY smoke against a REAL cheap OpenRouter model (NOT the fake LLM). This is
// a drift detector: it proves fleet still works end-to-end against a genuine
// provider — real tool-calling, real SSE, real sandbox execution. It is
// non-deterministic by nature (a real model) so it is NEVER a PR gate; it runs
// only in the secret-gated e2e-canary workflow (nightly/manual). When the
// OPENROUTER_API_KEY secret is absent the whole workflow is skipped upstream, so
// this spec assumes a real key + real-model backend (canary boot mode).
//
// The prompt demands a computation only run_python can answer correctly
// (sqrt of a non-trivial integer), so a right answer means the model genuinely
// called the tool and the sandbox genuinely executed it.

test.describe("canary: real model drives the real sandbox", () => {
  test("a real model computes a value via run_python in the sandbox", async ({ page, login }) => {
    test.setTimeout(180_000);
    await login();

    const composer = page.getByPlaceholder(/message .* ai/i);
    await composer.fill(
      "Use the run_python tool to compute and print round(math.sqrt(20264), 4) " +
        "(import math first). Then reply with just the printed number.",
    );
    await composer.press("Enter");

    // sqrt(20264) ≈ 142.3517. Match flexibly — a real model phrases freely.
    const conversation = page.getByRole("region", { name: "Conversation" });
    await expect(conversation).toContainText(/142\.351[67]/, { timeout: 150_000 });
    await expect(conversation).not.toContainText(/Mock reply to:/i);
  });
});
