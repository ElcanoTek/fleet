import { test, expect } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// LIVE edit + regenerate against the real turn loop. Both affordances re-run a
// turn through the REAL chat server + SSE (the fake LLM echoes the prompt token
// deterministically), and the edit path mutates the persisted user message — so
// this exercises real streaming + persistence, not a route mock.

const conversationRegion = (page: Page) => page.getByRole("region", { name: /conversation/i });

async function send(page: Page, marker: string) {
  const composer = page.getByPlaceholder(/message .* ai/i);
  await composer.fill(`[[echo:${marker}]]`);
  await composer.press("Enter");
}

// The composer carries the placeholder; the inline edit box is the other
// textarea, found by its current value.
async function editBoxWith(page: Page, value: string): Promise<Locator> {
  const areas = page.locator("textarea");
  await expect(async () => {
    const n = await areas.count();
    for (let i = 0; i < n; i++) {
      if ((await areas.nth(i).inputValue()).includes(value)) return;
    }
    throw new Error(`no edit textarea containing ${value} yet`);
  }).toPass({ timeout: 5_000 });
  const n = await areas.count();
  for (let i = 0; i < n; i++) {
    if ((await areas.nth(i).inputValue()).includes(value)) return areas.nth(i);
  }
  throw new Error(`edit textarea containing ${value} not found`);
}

test.describe("live edit + regenerate (real turn loop)", () => {
  test("Regenerate re-runs the same user turn without duplicating it", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await send(page, "alpha");
    const conv = conversationRegion(page);
    await expect(conv.getByText("alpha", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

    await page.getByRole("button", { name: /^Regenerate$/ }).click();

    // A fresh reply for the same turn streams again.
    await expect(conv.getByText("alpha", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

    // Regression guard: regenerate must re-run the SAME user turn, not append a
    // second copy of it — exactly one user bubble remains.
    await expect(conv.getByText("[[echo:alpha]]", { exact: true })).toHaveCount(1);
    await expect(conv).not.toContainText(/Mock reply to:/i);
  });

  test("Editing the last user message re-runs with the new text", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await send(page, "alpha");
    const conv = conversationRegion(page);
    await expect(conv.getByText("alpha", { exact: true }).first()).toBeVisible({ timeout: 60_000 });

    await page.getByRole("button", { name: /^Edit$/ }).click();
    const box = await editBoxWith(page, "echo:alpha");
    await box.fill("[[echo:bravo]]");
    await page.getByRole("button", { name: /^Resend$/i }).click();

    // The assistant replies to the EDITED prompt…
    await expect(conv.getByText("bravo", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
    // …and the original user message is gone from the transcript.
    await expect(conv.getByText("[[echo:alpha]]", { exact: true })).toHaveCount(0);
  });
});
