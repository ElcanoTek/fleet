import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the Patrol bar and the Patrols & timers dialog.
//
// A Raptive user clicked the bar's old "×" to dismiss it and it cancelled
// the patrol (a DELETE, no confirm). These pin the redesign end to end:
//
//   1. Hide folds the bar into the composer's clock with an Undo note, and
//      never touches the schedule; the choice survives a reload.
//   2. The dialog's Show bar switch brings a hidden bar back.
//   3. Stopping lives only in the dialog, and the DELETE goes out on the
//      second, confirming click — not the first.
//   4. A pending approval brings a hidden bar back with Review.
//   5. Review reaches a waiting card far up a long (virtualised) chat.

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

const NOW = new Date("2026-11-04T12:00:00Z");
const nowSec = Math.floor(NOW.getTime() / 1000);

const PATROL = {
  id: "sched-np",
  conversation_id: "conv-patrol",
  kind: "patrol",
  title: "Northpeak Campaign Watch",
  prompt: "Check Northpeak delivery and pacing against plan.",
  cron: "0 8 * * 1-5",
  timezone: "America/New_York",
  status: "active",
  run_count: 7,
  next_run_at: nowSec + 3600,
  last_run_at: nowSec - 600,
};

async function mockPatrolChat(page: Page, opts: { pending?: boolean; padding?: number } = {}) {
  const calls: string[] = [];
  let schedules = [PATROL];
  await page.clock.setFixedTime(NOW);
  await mockChatBoot(page, {
    conversations: [{ id: "conv-patrol", title: "Northpeak Campaign Watch" }],
  });
  await page.route("**/api/conversations/conv-patrol/schedules/**", (r: Route) => {
    calls.push(`${r.request().method()} ${new URL(r.request().url()).pathname}`);
    if (r.request().method() === "DELETE") schedules = [];
    return r.fulfill({
      json: { schedules, auto_approve: true, approval_mode: "auto" },
    });
  });
  await page.route("**/api/conversations/conv-patrol", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: {
          id: "conv-patrol",
          title: "Northpeak Campaign Watch",
          persona: "default",
          auto_approve: true,
          approval_mode: "auto",
        },
        schedules,
        history: [
          { id: 1, type: "text", role: "user", content: { text: "Watch Northpeak pacing." } },
          {
            id: 2,
            type: "tool_call",
            role: "assistant",
            content: { id: "tc-1", name: "dealdesk__update_deal", input: "{}" },
          },
          { id: 3, type: "text", role: "assistant", content: { text: "One change is staged." } },
          // Later turns that push the approval's message out of the
          // virtualised window.
          ...Array.from({ length: opts.padding ?? 0 }, (_, i) => ({
            id: 4 + i,
            type: "text",
            role: i % 2 === 0 ? "user" : "assistant",
            content: { text: `Pacing check ${i + 1}: delivery is on plan.\n\n${"Detail line. ".repeat(40)}` },
          })),
        ],
        pending_approvals: opts.pending
          ? [
              {
                approval_id: "ap-1",
                tool: "dealdesk__update_deal",
                summary: "Lower floor on PMP-4790",
                expires_at: nowSec + 86_000,
                tool_call_id: "tc-1",
              },
            ]
          : [],
      },
    });
  });
  await page.goto("/chat");
  await page.getByText("Northpeak Campaign Watch").first().click();
  return calls;
}

test("hiding the Patrol bar keeps the patrol and offers Undo", async ({ page }) => {
  const calls = await mockPatrolChat(page);
  const bar = page.getByTestId("patrol-banner");
  await expect(bar).toBeVisible({ timeout: 15_000 });
  await expect(bar.getByRole("button", { name: /cancel|stop|remove/i })).toHaveCount(0);

  await page.getByRole("button", { name: "Hide patrol bar. The patrol keeps running." }).click();
  await expect(bar).toBeHidden();
  await expect(page.getByTestId("patrol-hidden-hint")).toContainText("Patrol bar hidden");
  await expect(page.getByTestId("chat-patrol-button")).toHaveAttribute("data-patrol-bar-hidden", "true");

  await page.getByTestId("patrol-hidden-undo").click();
  await expect(bar).toBeVisible();

  // Hidden again, the choice outlives a reload.
  await page.getByTestId("patrol-bar-hide").click();
  await expect(bar).toBeHidden();
  await page.reload();
  await page.getByText("Northpeak Campaign Watch").first().click();
  await expect(page.getByText("One change is staged.")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("patrol-banner")).toBeHidden();

  // The dialog's switch shows it again.
  await page.getByTestId("chat-patrol-button").click();
  const dialog = page.getByRole("dialog", { name: "Patrols & timers" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("switch", { name: "Show bar above composer" }).click();
  await dialog.getByRole("button", { name: "Close patrols" }).click();
  await expect(page.getByTestId("patrol-banner")).toBeVisible();

  expect(calls).toEqual([]);
});

test("removing a patrol takes a second, confirming click", async ({ page }) => {
  const calls = await mockPatrolChat(page);
  await expect(page.getByTestId("patrol-banner")).toBeVisible({ timeout: 15_000 });
  await page.getByTestId("chat-patrol-button").click();
  const dialog = page.getByRole("dialog", { name: "Patrols & timers" });
  await dialog.getByTestId("patrols-card").getByRole("button", { expanded: false }).click();

  const remove = dialog.getByTestId("patrols-remove");
  await remove.click();
  await expect(remove).toHaveText("Confirm remove");
  expect(calls).toEqual([]);

  await remove.click();
  await expect.poll(() => calls).toEqual(["DELETE /api/conversations/conv-patrol/schedules/sched-np"]);
  await expect(page.getByTestId("patrol-banner")).toBeHidden();
});

test("a waiting approval brings a hidden bar back with Review", async ({ page }) => {
  await page.addInitScript(() => {
    window.localStorage.setItem("fleet.patrolBarHidden.conv-patrol", "1");
  });
  await mockPatrolChat(page, { pending: true });
  const bar = page.getByTestId("patrol-banner");
  await expect(bar).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("patrol-bar-attention")).toContainText(
    "An action is waiting for your approval",
  );
  await expect(bar.getByRole("button", { name: "Review" })).toBeVisible();
});

test("Review reaches a waiting card that is far up a long chat", async ({ page }) => {
  await mockPatrolChat(page, { pending: true, padding: 80 });
  const bar = page.getByTestId("patrol-banner");
  await expect(bar).toBeVisible({ timeout: 15_000 });
  const card = page.locator('[data-approval-id="ap-1"]');
  // The chat opens at its end, so the card's message is not mounted.
  await expect(page.getByText(/Pacing check 80:/)).toBeVisible({ timeout: 15_000 });
  await expect(card).toHaveCount(0);
  // A loaded history is put at its end once more, 80 ms after the first
  // frame (chat-experience.tsx). A person can't click inside that window;
  // let it pass so the test doesn't either.
  await page.waitForTimeout(300);

  await bar.getByRole("button", { name: "Review" }).click();
  await expect(card).toBeInViewport({ timeout: 5_000 });
});

test("starting a patrol in an Autonomous chat asks once, then creates it", async ({ page }) => {
  // The owner already chose Autonomous; this viewer has not accepted the
  // risk. Accepting must create the patrol, not reopen the prompt.
  const creates: unknown[] = [];
  let accepts = 0;
  await page.clock.setFixedTime(NOW);
  await mockChatBoot(page, {
    conversations: [{ id: "conv-patrol", title: "Northpeak Campaign Watch" }],
  });
  await page.route("**/api/chat/autonomous-accept", (r: Route) => {
    accepts += 1;
    return r.fulfill({ json: { ok: true } });
  });
  await page.route("**/api/conversations/conv-patrol/schedules", (r: Route) => {
    if (r.request().method() !== "POST") return r.fulfill({ json: { schedules: [] } });
    creates.push(r.request().postDataJSON());
    return r.fulfill({
      json: {
        schedule: { ...PATROL, approval_mode: "autonomous" },
        schedules: [PATROL],
        auto_approve: true,
        approval_mode: "autonomous",
      },
    });
  });
  await page.route("**/api/conversations/conv-patrol", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    return r.fulfill({
      json: {
        conversation: {
          id: "conv-patrol",
          title: "Northpeak Campaign Watch",
          persona: "default",
          auto_approve: true,
          approval_mode: "autonomous",
        },
        autonomous_accepted: false,
        schedules: [],
        history: [{ id: 1, type: "text", role: "user", content: { text: "Watch Northpeak pacing." } }],
        pending_approvals: [],
      },
    });
  });
  await page.goto("/chat");
  await page.getByText("Northpeak Campaign Watch").first().click();
  await expect(page.getByText("Watch Northpeak pacing.")).toBeVisible({ timeout: 15_000 });

  // No patrols yet: the dialog opens from the composer's More menu.
  await page.getByTestId("composer-more-button").click();
  await page.getByTestId("composer-more-popover").getByTestId("chat-patrol-button").click();
  const dialog = page.getByRole("dialog", { name: "Patrols & timers" });
  await dialog.getByRole("button", { name: "Start patrol" }).click();
  const risk = page.getByTestId("autonomous-risk-modal");
  await expect(risk).toBeVisible();
  expect(creates).toHaveLength(0);

  await page.getByTestId("autonomous-risk-accept").click();
  await expect.poll(() => creates.length).toBe(1);
  await expect(risk).toBeHidden();
  expect(accepts).toBe(1);
  expect(creates[0]).toMatchObject({ approval_mode: "autonomous" });
});
