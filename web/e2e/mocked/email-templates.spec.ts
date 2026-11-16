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
  has_preview: true,
  fields: [
    { key: "campaign", label: "Campaign", type: "text", required: true },
    { key: "notes", label: "Anything to stress or leave out", type: "textarea", advanced: true },
  ],
  prompt_template:
    "Write this week's client optimization update for {campaign} with the email template weekly-optimization-update.\nNotes: {notes}",
};

// The list carries no preview; the dialog reads the one on screen.
const weeklyPreview =
  '<!doctype html><html><body style="font-family:sans-serif"><h1>Acme Outfitters</h1><p>Hi Team,</p><script>document.title="ran"</script></body></html>';

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
  await page.route("**/api/orchestrator/email-templates/weekly-optimization-update", (r: Route) =>
    r.fulfill({ json: { ...weekly, preview_html: weeklyPreview } }),
  );

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

test("the form picks who it goes to and how it looks, and asks for a draft", async ({ page }) => {
  const withPickers = {
    ...weekly,
    fields: [
      { key: "campaign", label: "Campaign", type: "text", required: true },
      { key: "recipient", label: "Send to", type: "recipients", hint: "Leave empty for a review copy to yourself." },
      {
        key: "style",
        label: "Style",
        type: "palette",
        options: ["elcano", "amazon"],
        swatches: { elcano: ["#1A0B1E", "#6262A0"], amazon: ["#333E48", "#FF9900"] },
        custom: ["Header", "Accent"],
        default: "elcano",
      },
    ],
    prompt_template:
      "Write this week's update for {campaign}.\nSend to: {recipient}\nStyle: {style}\nThis is a draft: show me the preview and wait.",
  };
  await mockChatBoot(page);
  await page.route("**/api/orchestrator/email-templates", (r: Route) => r.fulfill({ json: [withPickers] }));
  await page.route("**/api/orchestrator/email-templates/weekly-optimization-update", (r: Route) =>
    r.fulfill({ json: { ...withPickers, preview_html: weeklyPreview } }),
  );
  await page.route("**/api/orchestrator/tasks/recipients", (r: Route) =>
    r.fulfill({
      json: [
        { email: "media@acme.com", task_count: 5, last_used: "2026-11-10T00:00:00Z" },
        { email: "trading@elcanotek.com", task_count: 2, last_used: "2026-11-09T00:00:00Z" },
      ],
    }),
  );

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "More options" }).click();
  await page.getByRole("menuitem", { name: "Open email templates" }).click();
  const dialog = page.getByRole("dialog", { name: "Email templates" });
  await dialog.getByRole("button", { name: "Use this template" }).click();
  await dialog.getByRole("textbox", { name: /^Campaign/ }).fill("Acme CTV");

  // The team the person's tasks already mail is one click each; anyone else is typed.
  const offered = dialog.getByTestId("recipient-suggestions");
  await offered.getByRole("button", { name: "media@acme.com" }).click();
  await dialog.getByPlaceholder("Add an address").fill("new.buyer@acme.com");
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  await expect(dialog.getByTestId("recipient-chosen").getByRole("listitem")).toHaveCount(2);

  await dialog.getByRole("radio", { name: "amazon" }).click();
  await dialog.getByRole("radio", { name: "Custom colors" }).click();
  await dialog.getByLabel("Accent", { exact: true }).fill("#c2410c");
  if (process.env.E2E_PICKER_SHOT) await page.screenshot({ path: process.env.E2E_PICKER_SHOT });
  await dialog.getByRole("button", { name: "Use prompt" }).click();

  await expect(page.getByRole("textbox").first()).toHaveValue(
    "Write this week's update for Acme CTV.\nSend to: media@acme.com, new.buyer@acme.com\nStyle: custom (Header #1F3A5F, Accent #C2410C)\nThis is a draft: show me the preview and wait.",
  );

  // Next time, the typed address and the look are offered again.
  await page.getByRole("button", { name: "More options" }).click();
  await page.getByRole("menuitem", { name: "Open email templates" }).click();
  await dialog.getByRole("button", { name: "Use this template" }).click();
  await expect(dialog.getByTestId("recipient-suggestions").getByRole("button", { name: "new.buyer@acme.com" })).toBeVisible();
  await expect(dialog.getByTestId("palette-recent").getByRole("button", { name: /Header #1F3A5F, Accent #C2410C/ })).toBeVisible();
});
