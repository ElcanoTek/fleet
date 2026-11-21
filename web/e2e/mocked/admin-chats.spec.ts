import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";
import { ADMIN, DANA, STUCK_ID, mockAdminChats } from "./_adminChats";

// Settings → Admin → Chats (ADR-0115, ADR-0117), in a browser: the overview
// that says who needs help, a chat opened from it with the conversation as its
// owner sees it, pinning a note, and that note in the owner's own chat. Runs
// at desktop and phone widths: the page must not scroll sideways at either.

const NOW = Math.floor(Date.now() / 1000);

async function noSidewaysScroll(page: import("@playwright/test").Page) {
  const overflow = await page.evaluate(() => {
    const main = document.querySelector("main");
    return {
      doc: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      main: main ? main.scrollWidth - main.clientWidth : 0,
    };
  });
  expect(overflow.doc, "the document scrolls sideways").toBeLessThanOrEqual(1);
  expect(overflow.main, "the page column scrolls sideways").toBeLessThanOrEqual(1);
}

async function expectNoViolations(page: import("@playwright/test").Page) {
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"]).analyze();
  const summary = results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(" | ")}`);
  expect(summary, summary.join("\n")).toEqual([]);
}

for (const size of [
  { name: "desktop", width: 1280, height: 900 },
  { name: "phone", width: 390, height: 844 },
]) {
  test.describe(`admin chats · ${size.name}`, () => {
    test.use({ viewport: { width: size.width, height: size.height } });

    test("from who needs help to a pinned note", async ({ page, context }) => {
      await loginViaCookie(context, ADMIN);
      const mock = await mockAdminChats(page);
      await page.goto("/settings/admin/chats");

      // People who need attention come first, with what they are waiting on.
      const people = page.getByTestId("admin-person");
      await expect(people).toHaveCount(3);
      await expect(people.first()).toContainText(DANA);
      await expect(people.first()).toContainText("2 need attention");
      await expect(page.getByTestId("tile-attention")).toContainText("2");
      await noSidewaysScroll(page);
      await expectNoViolations(page);

      // The tile narrows the page to the chats that need someone.
      await page.getByTestId("tile-attention").click();
      await expect(people).toHaveCount(1);
      await expect(page.getByTestId("admin-chat-row")).toHaveCount(2);
      await page.getByTestId("admin-chat-row").filter({ hasText: "Weekly client report" }).click();

      // The chat: what it is waiting on, and the conversation read-only.
      await expect(page).toHaveURL(new RegExp(`chat=${STUCK_ID}`));
      await expect(page.getByTestId("admin-chat-attention")).toContainText(`Waiting on ${DANA}`);
      const transcript = page.getByTestId("admin-chat-transcript");
      await expect(transcript.getByTestId("admin-transcript-user").first()).toContainText("this week's client report");
      await expect(transcript.getByRole("table")).toBeVisible();
      await expect(transcript.getByTestId("patrol-turn-notice")).toBeVisible();
      await expect(page.getByText(`As ${DANA} sees it · read-only`)).toBeVisible();
      await noSidewaysScroll(page);

      // Pin a note: it is sent as written and shown as not yet acknowledged.
      await page.getByTestId("admin-note-text").fill("Approve the card under the last reply to send the report.");
      await page.getByTestId("admin-note-pin").click();
      await expect(page.getByTestId("admin-pinned-note")).toContainText("Approve the card under the last reply");
      await expect(page.getByTestId("admin-note-unseen")).toBeVisible();
      await expectNoViolations(page);
      expect(mock.calls.find((c) => c.method === "PUT")?.body).toEqual({
        text: "Approve the card under the last reply to send the report.",
        expect_updated_at: 0,
      });

      // A message to the assistant says who sees it and that it is read.
      await page.getByRole("button", { name: "Message the assistant" }).click();
      await expect(page.getByTestId("step-in-reach")).toContainText("The assistant reads it and replies");
      await noSidewaysScroll(page);

      // The other tabs.
      await page.getByTestId("admin-tab-patrols").click();
      await expect(page.getByRole("heading", { name: "Monday report" })).toBeVisible();
      await page.getByTestId("admin-tab-settings").click();
      await expect(page.getByRole("heading", { name: "Archive or delete" })).toBeVisible();
      await noSidewaysScroll(page);
      await page.getByTestId("admin-tab-activity").click();
      await expect(page.getByTestId("admin-chat-activity")).toContainText("Pinned a note");

      // Back to the overview, as it was left.
      await page.getByTestId("admin-chat-back").click();
      await expect(page.getByTestId("tile-attention")).toHaveAttribute("aria-pressed", "true");
    });

    test("the owner sees the pinned note above the conversation and acknowledges it", async ({ page, context }) => {
      await loginViaCookie(context, DANA);
      await mockChatBoot(page, { conversations: [{ id: "conv-report", title: "Weekly client report" }] });
      let seen = false;
      let acked: unknown = null;
      const note = () => ({
        text: "Approve the card under the last reply to send the report.",
        author: ADMIN,
        updated_at: NOW - 600,
        ...(seen ? { seen_at: NOW, seen_by: DANA } : {}),
      });
      await page.route("**/api/conversations/conv-report/admin-note", (r: Route) => {
        // GET re-reads the note; POST is "Got it".
        if (r.request().method() === "POST") {
          acked = JSON.parse(r.request().postData() ?? "{}");
          seen = true;
        }
        return r.fulfill({ json: { admin_note: note() } });
      });
      await page.route("**/api/conversations/conv-report", (r: Route) =>
        r.fulfill({
          json: {
            conversation: { id: "conv-report", title: "Weekly client report", persona: "default", model: "m", user_email: DANA, created_at: NOW - 86400, updated_at: NOW - 60 },
            history: [
              { id: 1, role: "user", type: "text", content: { text: "Put together this week's report." } },
              { id: 2, role: "assistant", type: "text", content: { text: "Drafted. The email needs your approval." } },
            ],
            pending_approvals: [],
            resolved_approvals: [],
            pending_memory_proposals: [],
            schedules: [],
            ui_autosubmits: [],
            admin_note: note(),
          },
        }),
      );
      await page.goto("/chat?c=conv-report");

      const banner = page.getByTestId("admin-note-banner");
      await expect(banner).toContainText("Approve the card under the last reply");
      await expect(banner).toContainText("The assistant doesn’t read it");
      await noSidewaysScroll(page);
      await expectNoViolations(page);

      await page.getByTestId("admin-note-ack").click();
      // Acknowledged: one line, still pinned, and the server was told which
      // text was read.
      await expect(banner).toHaveAttribute("aria-expanded", "false");
      expect(acked).toEqual({ updated_at: NOW - 600 });
      await banner.click();
      await expect(page.getByRole("note", { name: "Note from an administrator" })).toBeVisible();
    });
  });
}
