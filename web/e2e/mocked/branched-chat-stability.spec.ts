import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

test("a teammate branch with unavailable generated images settles at the bottom after load and reload", async ({ page, context }) => {
  await loginViaCookie(context);
  await page.route("**/api/**", route => route.fulfill({ json: {} }));
  const conversation = { id: "branched-images", title: "Image report (from teammate)", persona: "default", model: "test-model" };
  await mockChatBoot(page, { conversations: [conversation] });
  // Teammate copies retain text (including Markdown image references), but
  // exclude tool rows and workspace files. Old copies can also contain empty
  // text rows left by stripped image attachments.
  const history = Array.from({ length: 12 }, (_, i) => [
    { id: i * 3 + 1, role: "user", type: "text", content: { text: `Review chart ${i + 1}` } },
    { id: i * 3 + 2, role: "user", type: "text", content: { text: "" } },
    { id: i * 3 + 3, role: "assistant", type: "text", content: { text: `## Report ${i + 1}\n\nThe generated chart shows our progress.\n\n![Generated chart ${i + 1}](chart-${i + 1}.png)\n\nReview the chart with the team.` } },
  ]).flat();
  await page.route("**/api/conversations/branched-images", route => route.fulfill({ json: { conversation, history } }));
  let imageRequests = 0;
  await page.route("**/api/conversations/branched-images/workspace/**", async route => {
    imageRequests++;
    await route.fulfill({ status: 404, body: "Not found" });
  });
  await page.goto("/chat");
  for (const reload of [false, true]) {
    if (reload) await page.reload();
    const transcript = page.getByRole("region", { name: "Conversation" });
    await expect(transcript.getByText("Report 12", { exact: true })).toBeVisible();
    await transcript.evaluate(el => { el.scrollTop = el.scrollHeight; });
    await expect(transcript.getByText(/couldn.t load image: Generated chart 12/)).toBeVisible();
    // Let the initial lazy Markdown load and the one failed image request
    // settle, then sample the actual browser layout every animation frame.
    await page.waitForTimeout(700);
    const requestsBefore = imageRequests;
    const samples = await transcript.evaluate(async el => {
      const points: number[][] = [];
      for (let i = 0; i < 90; i++) {
        await new Promise(requestAnimationFrame);
        points.push([el.scrollTop, el.scrollHeight]);
      }
      return points;
    });
    await test.info().attach(reload ? "layout-after-reload" : "layout-after-load", {
      body: JSON.stringify({ samples, extraImageRequests: imageRequests - requestsBefore }),
      contentType: "application/json",
    });
    expect(imageRequests - requestsBefore, "settled images must not be retried by measurement renders").toBe(0);
    for (const axis of [0, 1]) {
      const values = samples.map(point => point[axis]);
      expect(Math.max(...values) - Math.min(...values), `layout axis ${axis}`).toBeLessThanOrEqual(1);
    }
  }
});
