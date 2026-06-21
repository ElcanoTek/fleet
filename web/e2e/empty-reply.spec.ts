import { test, expect } from "./fixtures";

// Covers the "agent stopped without a written reply" safety net end-to-end
// against the MOCKED chat-server. The prompt "simulate empty reply" makes the
// mock turn run a tool call and then complete with NO assistant text — the
// Husqvarna / bugreport.txt / bugreport2.txt failure shape. The UI must show a
// clear notice + Retry instead of a blank assistant bubble.
//
// (The real server forces a summary in this case — verified by the agent
// package's live test — but mock mode short-circuits RunTurn, so what this
// locks in is the front-end fallback that also covers older persisted empty
// turns.)
test.describe("empty assistant reply", () => {
  test("shows a 'finished without a written reply' notice + Retry", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("simulate empty reply");
    await composer.press("Enter");

    // The fallback notice stands in for the missing answer.
    await expect(page.getByText(/finished without a written reply/i)).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByRole("button", { name: /^retry$/i })).toBeVisible();

    // And it must NOT have rendered an empty "Mock reply to:" answer.
    await expect(page.getByText(/Mock reply to:/i)).toHaveCount(0);
  });

  test("the notice survives a reload (persisted empty turn)", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("simulate empty reply");
    await composer.press("Enter");
    await expect(page.getByText(/finished without a written reply/i)).toBeVisible({
      timeout: 15_000,
    });

    // Reload replays history through historyToMessages — the empty-content
    // done turn must still surface the notice, not a blank bubble.
    await page.reload();
    await expect(page.getByText(/finished without a written reply/i)).toBeVisible({
      timeout: 15_000,
    });
  });
});
