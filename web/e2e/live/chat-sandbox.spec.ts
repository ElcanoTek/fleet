import { test, expect } from "./fixtures";

// LIVE chat journey against the fully real stack. The fake LLM (scenario
// "tool-loop") drives a genuine multi-step tool loop:
//   turn 0 → bash       → fleet runs `echo FAKELLM_BASH_OK` in the real Podman
//                         sandbox and streams the real stdout back over SSE,
//   turn 1 → run_python → fleet runs Python in the real IPython kernel inside
//                         the sandbox and streams the real result,
//   turn 2 → final text incorporating both tool outputs.
//
// Everything here is real except the LLM: real auth, real chat-server, real
// SSE, real container execution. The only determinism lever is the scenario
// marker in the prompt.

test.describe("live chat → real sandbox tool loop", () => {
  test("fake LLM drives bash + run_python in the real sandbox; output streams over SSE", async ({
    page,
    login,
  }) => {
    // Cold container start + two sandbox tool calls can exceed the 60s default.
    test.setTimeout(150_000);
    await login();

    const composer = page.getByPlaceholder(/message .* ai/i);
    await composer.fill("run the sandbox loop [[scenario:tool-loop]]");
    await composer.press("Enter");

    const conversation = page.getByRole("region", { name: "Conversation" });

    // The final assistant text is emitted only after BOTH tool calls have run
    // in the sandbox and their results fed back — so asserting it proves the
    // whole real loop completed.
    await expect(conversation).toContainText(
      /Sandbox run complete: bash said FAKELLM_BASH_OK and python computed FAKELLM_PY_RESULT 42\./i,
      { timeout: 90_000 },
    );

    // Guard against silently running on the mock harness or a misconfigured LLM.
    await expect(conversation).not.toContainText(/Mock reply to:/i);

    // The execution trail (tool chips) is revealed via "Show details" (the
    // fixture pre-seeds the toggle; click as a fallback if the chip is hidden).
    const bashChip = page.getByRole("button", { name: /bash/i });
    const pyChip = page.getByRole("button", { name: /run_python/i });
    if ((await bashChip.count()) === 0) {
      const toggle = page.getByRole("button", { name: /show details/i });
      if (await toggle.isVisible().catch(() => false)) await toggle.click();
    }
    await expect(bashChip.first()).toBeVisible({ timeout: 15_000 });
    await expect(pyChip.first()).toBeVisible();

    // Expand the run_python chip and assert the REAL stdout captured from the
    // sandbox kernel is rendered (not a canned string). The result renders in a
    // <pre> block; scope to it so we don't also match the final assistant text
    // (which echoes the same marker).
    await pyChip.first().click();
    const resultBlock = page.locator("pre", { hasText: /FAKELLM_PY_RESULT 42/ });
    await expect(resultBlock.first()).toBeVisible({ timeout: 10_000 });
    // And it carries the real sandbox JSON envelope (status:success), proving
    // this is genuine container stdout, not a UI string.
    await expect(resultBlock.first()).toContainText(/"status":"success"/);
  });
});
