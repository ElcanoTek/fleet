import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Transfer ownership (docs/CHAT-TRANSFER.md): the owner hands a chat to
// another person from its row menu; an admin can do the same from a Team
// chats row. The server is mocked, so this pins the dialog: what it says
// before the click, what it sends, and what the page does afterwards.

const OWN = { id: "own1", title: "Reklaim Opti workbench", user_email: "e2e@example.com", multiplayer: true };
const TEAMMATE = { id: "mate1", title: "Client handover board", multiplayer: true, user_email: "jeanne@example.com" };

async function boot(page: Page, opts: { admin?: boolean } = {}) {
  await mockChatBoot(page, { conversations: [OWN, TEAMMATE] });
  await page.route("**/api/me", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com", role: opts.admin ? "admin" : "member" } }),
  );
  await page.route("**/api/admin/settings", (r: Route) =>
    opts.admin ? r.fulfill({ json: {} }) : r.fulfill({ status: 403, body: "forbidden" }),
  );
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [] } }));
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: [] } }));
  await page.route("**/api/sidebar", (r: Route) => r.fulfill({ json: { layout: null, workspace_default: null } }));
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the owner transfers a chat from its row menu", async ({ page }) => {
  await boot(page);
  const sent: unknown[] = [];
  let answer: "refuse" | "accept" = "refuse";
  await page.route("**/api/conversations/own1/transfer", (r: Route) => {
    sent.push(r.request().postDataJSON());
    if (answer === "refuse") {
      return r.fulfill({
        status: 400,
        contentType: "text/plain",
        body: "that person can't take this chat: they need an active Fleet account that can chat\n",
      });
    }
    return r.fulfill({
      json: {
        conversation_id: "own1",
        owner_email: "sam@reklaim.example",
        previous_owner: "e2e@example.com",
        patrols_moved: 2,
        patrols_paused: 1,
      },
    });
  });
  await page.goto("/chat");

  await page.getByRole("button", { name: "Conversation options for Reklaim Opti workbench" }).click();
  await page.getByRole("menuitem", { name: "Transfer ownership…", exact: true }).click();
  const dialog = page.getByTestId("transfer-chat-dialog");
  await expect(dialog).toBeVisible();
  // It says what goes with the chat and what the owner loses, before the click.
  await expect(dialog).toContainText("patrols and timers move with it and are paused");
  await expect(dialog).toContainText("You lose access to it");
  const confirm = dialog.getByTestId("transfer-chat-confirm");
  await expect(confirm).toBeDisabled();
  await expect(dialog.getByTestId("transfer-chat-email")).toBeFocused();

  await dialog.getByTestId("transfer-chat-email").fill("  sam@reklaim.example ");
  // A refusal is shown in the dialog, which stays open with the address.
  await confirm.click();
  await expect(dialog.getByTestId("transfer-chat-error")).toContainText("can't take this chat");
  await expect(dialog.getByTestId("transfer-chat-email")).toHaveValue("sam@reklaim.example");
  await expect(page.getByRole("button", { name: "Conversation options for Reklaim Opti workbench" })).toHaveCount(1);

  answer = "accept";
  await confirm.click();
  const done = page.getByTestId("transfer-chat-done");
  await expect(done).toContainText("now belongs to sam@reklaim.example");
  await expect(done).toContainText("Its patrols and timers are paused until the new owner resumes them.");
  expect(sent).toEqual([{ to_email: "sam@reklaim.example" }, { to_email: "sam@reklaim.example" }]);
  // The chat is no longer this person's: its row is gone.
  await expect(page.getByRole("button", { name: "Conversation options for Reklaim Opti workbench" })).toHaveCount(0);
  await done.getByRole("button", { name: "Done", exact: true }).click();
  await expect(done).toHaveCount(0);
});

test("a Lockdown chat does not offer the transfer", async ({ page }) => {
  await mockChatBoot(page, { conversations: [{ ...OWN, id: "locked1", title: "Sealed deal room", lockdown: true, multiplayer: false }] });
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ status: 403, body: "forbidden" }));
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [] } }));
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: [] } }));
  await page.route("**/api/sidebar", (r: Route) => r.fulfill({ json: { layout: null, workspace_default: null } }));
  await page.goto("/chat");
  await page.getByRole("button", { name: "Conversation options for Sealed deal room" }).click();
  const menu = page.getByRole("menu", { name: "Options for Sealed deal room" });
  await expect(menu.getByRole("menuitem", { name: "Share…", exact: true })).toBeVisible();
  await expect(menu.getByRole("menuitem", { name: "Transfer ownership…", exact: true })).toHaveCount(0);
});

test("a Team chats row offers the transfer to an admin only", async ({ page }) => {
  await boot(page);
  await page.goto("/chat");
  const row = page.getByTestId("team-shared-conv-row").filter({ hasText: "Client handover board" }).locator("..");
  await row.hover();
  await row.getByTestId("team-chat-options").click();
  await expect(page.getByRole("menu", { name: "Options for Client handover board" }).getByRole("menuitem")).toHaveText([
    "Pin",
    "Add to sidebar",
    "Copy link",
    "Download chat…",
  ]);
});

test("an admin transfers a teammate's chat from Team chats", async ({ page }) => {
  await boot(page, { admin: true });
  let body: unknown = null;
  await page.route("**/api/conversations/mate1/transfer", (r: Route) => {
    body = r.request().postDataJSON();
    return r.fulfill({
      json: { conversation_id: "mate1", owner_email: "sam@reklaim.example", patrols_moved: 0, patrols_paused: 0 },
    });
  });
  await page.goto("/chat");
  const row = page.getByTestId("team-shared-conv-row").filter({ hasText: "Client handover board" }).locator("..");
  await row.hover();
  await row.getByTestId("team-chat-options").click();
  await page.getByRole("menuitem", { name: "Transfer ownership…", exact: true }).click();
  const dialog = page.getByTestId("transfer-chat-dialog");
  await dialog.getByTestId("transfer-chat-email").fill("sam@reklaim.example");
  await dialog.getByTestId("transfer-chat-confirm").click();
  await expect(page.getByTestId("transfer-chat-done")).toContainText("It has no patrols or timers.");
  expect(body).toEqual({ to_email: "sam@reklaim.example" });
  await expect(page.getByTestId("team-shared-conv-row").filter({ hasText: "Client handover board" })).toHaveCount(0);
});

test("a chat an admin transfers to themselves is reloaded into their own list", async ({ page }) => {
  await boot(page, { admin: true });
  await page.route("**/api/conversations/mate1/transfer", (r: Route) =>
    r.fulfill({
      json: { conversation_id: "mate1", owner_email: "E2E@example.com", patrols_moved: 0, patrols_paused: 0 },
    }),
  );
  await page.goto("/chat");
  const row = page.getByTestId("team-shared-conv-row").filter({ hasText: "Client handover board" }).locator("..");
  await row.hover();
  await row.getByTestId("team-chat-options").click();
  await page.getByRole("menuitem", { name: "Transfer ownership…", exact: true }).click();
  const dialog = page.getByTestId("transfer-chat-dialog");
  await dialog.getByTestId("transfer-chat-email").fill("e2e@example.com");
  // The chat is now the admin's own, so the page asks for its list again
  // rather than only dropping the row.
  const reloaded = page.waitForRequest(
    (req) => req.method() === "GET" && new URL(req.url()).pathname === "/api/conversations",
  );
  await dialog.getByTestId("transfer-chat-confirm").click();
  await expect(page.getByTestId("transfer-chat-done")).toBeVisible();
  await reloaded;
});

// Contrast is per theme, so axe runs over the dialog in both colour schemes.
for (const scheme of ["light", "dark"] as const) {
  test(`the transfer dialog has no axe violations (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme });
    await boot(page);
    await page.goto("/chat");
    await page.getByRole("button", { name: "Conversation options for Reklaim Opti workbench" }).click();
    await page.getByRole("menuitem", { name: "Transfer ownership…", exact: true }).click();
    const dialog = page.getByTestId("transfer-chat-dialog");
    await dialog.getByTestId("transfer-chat-email").fill("sam@reklaim.example");
    // Let the entrance animation settle so contrast is measured at rest.
    await page.waitForTimeout(400);
    const results = await new AxeBuilder({ page })
      .include('[data-testid="transfer-chat-dialog"]')
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();
    const summary = results.violations.map(
      (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    );
    expect(summary, summary.join("\n")).toEqual([]);
  });
}
