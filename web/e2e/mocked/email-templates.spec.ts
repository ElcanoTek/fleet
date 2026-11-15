import { test, expect } from "@playwright/test";
import type { Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the Email templates library (docs/EMAIL-TEMPLATES.md): the
// composer's menu lists it only when the bundle ships templates, the dialog
// shows the bundle's example beside each section's rules, and Use prompt puts
// the filled-in request in the composer without sending anything.

const weekly = {
  id: "weekly-optimization-update",
  name: "Weekly optimization update",
  description: "The trader's weekly client note.",
  audience: "client",
  use_when: "A campaign has DSP reporting and the client gets a weekly note.",
  path: "email_templates/weekly-optimization-update.yaml",
  max_words: 260,
  subject: { instruction: "Campaign, channel, the one result that matters.", max_chars: 70 },
  rules: ["Write for a client who gives this one minute."],
  never_say: ["no optimizations", "re:\\bELC\\d{3,}\\b"],
  sections: [
    {
      id: "summary",
      title: "Summary",
      heading: "Summary",
      block: "text",
      required: true,
      instruction: "Greeting, one framing sentence, then up to three bullets.",
      limits: { max_paragraphs: 2, max_bullets: 3, max_words: 100 },
    },
    {
      id: "glance",
      title: "This week at a glance",
      block: "headline_metrics",
      required: true,
      instruction: "Three or four numbers, the primary KPI first.",
      limits: { min_items: 3, max_items: 4 },
    },
    { id: "trend", title: "Trend chart", block: "chart", required: false, instruction: "One chart." },
  ],
  preview_html:
    '<!doctype html><html><body style="font-family:sans-serif"><h1>Acme Outfitters</h1><p>Hi Team,</p><script>document.title="ran"</script></body></html>',
  fields: [
    { key: "campaign", label: "Campaign", type: "text", required: true },
    { key: "notes", label: "Anything to stress or leave out", type: "textarea", advanced: true },
  ],
  prompt_template:
    "Write this week's client optimization update for {campaign} with the email template weekly-optimization-update.\nNotes: {notes}",
};

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the composer menu has no Email templates entry when the bundle ships none", async ({ page }) => {
  await mockChatBoot(page);
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "More options" }).click();
  await expect(page.getByRole("menuitem", { name: "Open prompt library" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "Open email templates" })).toHaveCount(0);
});

test("an email template shows its example and rules, and its form fills the composer", async ({ page }) => {
  await mockChatBoot(page);
  await page.route("**/api/orchestrator/email-templates", (r: Route) => r.fulfill({ json: [weekly] }));

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "More options" }).click();
  await page.getByRole("menuitem", { name: "Open email templates" }).click();

  const dialog = page.getByRole("dialog", { name: "Email templates" });
  await expect(dialog).toBeVisible();

  // The bundle's sample renders in a frame that cannot run its script.
  const frame = dialog.frameLocator('iframe[title="Example of Weekly optimization update"]');
  await expect(frame.getByText("Hi Team,")).toBeVisible();
  await expect(dialog.locator("iframe")).toHaveAttribute("sandbox", "");
  expect(await frame.locator("html").evaluate(() => document.title)).not.toBe("ran");

  const sections = dialog.getByTestId("email-template-sections");
  await expect(sections.getByText("1. Summary")).toBeVisible();
  await expect(sections.getByText("Up to 100 words · Up to 2 paragraphs · Up to 3 bullets")).toBeVisible();
  await expect(sections.getByText("3 to 4 numbers")).toBeVisible();
  await expect(sections.getByText("Optional")).toBeVisible();
  await expect(dialog.getByText("For clients")).toBeVisible();
  await expect(dialog.getByText("no optimizations")).toBeVisible();

  await dialog.getByRole("button", { name: "Use this template" }).click();
  const use = dialog.getByRole("button", { name: "Use prompt" });
  await expect(use).toBeDisabled();
  await dialog.getByRole("textbox", { name: /^Campaign/ }).fill("Acme CTV");
  await use.click();

  await expect(dialog).toBeHidden();
  await expect(page.getByRole("textbox").first()).toHaveValue(
    "Write this week's client optimization update for Acme CTV with the email template weekly-optimization-update.",
  );
});
