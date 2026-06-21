import { test, expect } from "./fixtures";

// Edit + Regenerate affordances on the most-recent user and assistant
// messages. Both re-run the turn through the mocked server, so the
// assertions look for a new "Mock reply to:" line.

test.describe("edit + regenerate", () => {
  test("Regenerate re-runs the last assistant reply", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("one");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*one/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Regenerate button sits under the assistant reply.
    await page.getByRole("button", { name: /^Regenerate$/ }).click();

    // After regenerate, a fresh "Mock reply to: one" streams again. The
    // text can be rendered twice during the transition (original fading,
    // new streaming in); we only require that a reply for "one" is on
    // screen after the click resolves.
    await expect(page.getByText(/Mock reply to:\s*one/i).last()).toBeVisible({
      timeout: 15_000,
    });

    // Regression guard: regenerate must re-run the SAME user turn, not
    // append a second copy of it. The old retry path kept the user
    // bubble client-side and re-submitted it, leaving two identical
    // "one" bubbles (and persisting the prompt twice). Exactly one user
    // bubble for "one" must remain in the transcript.
    const conversation = page.getByRole("region", { name: /conversation/i });
    await expect(conversation.getByText("one", { exact: true })).toHaveCount(1);
  });

  test("Edit the last user message re-runs with the new text", async ({ page, login }) => {
    await login();

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("draft A");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*draft A/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // The Edit action sits in an always-visible footer under the last
    // user bubble (no hover-reveal), so it's clickable straight away.
    await page.getByRole("button", { name: /^Edit$/ }).click();

    // Textarea appears. Clear and type a new message, then resend.
    const editTextarea = page.locator("textarea").filter({ hasText: "draft A" });
    await editTextarea.fill("draft B");
    await page.getByRole("button", { name: /^Resend$/i }).click();

    // Assistant replies to the EDITED prompt.
    await expect(page.getByText(/Mock reply to:\s*draft B/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // And the original text should no longer appear in the conversation
    // transcript. (The sidebar row title stays "draft A" until the
    // auto-title job fires — that's intentional and outside this test.)
    const conversation = page.getByRole("region", { name: /conversation/i });
    await expect(conversation.getByText("draft A", { exact: true })).toHaveCount(0);
  });
});
