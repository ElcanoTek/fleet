import { test, expect } from "./fixtures";
import type { Locator, Page } from "@playwright/test";

// LIVE conversation-management journeys against the fully booted stack. Pinning,
// deleting, bulk-delete and history resume all persist through the REAL chat
// server + Postgres — not a route mock — so these prove the persistence layer,
// not just the React store. Each chat is seeded with a distinct "[[echo:TITLE]]"
// reply, so the fake LLM gives every conversation its own first-assistant reply
// and therefore its own sidebar title; that lets the per-row "Pin <title>" /
// "Delete <title>" controls be addressed unambiguously.
//
// In the unified rail (#169) the per-row actions live in a kebab menu (Pin ·
// Rename · Download · Add to folder · Labels · Archive · Delete) rather than as
// inline hover buttons. openRowMenu opens that menu; the action's accessible
// name ("Pin <title>", "Archive <title>", "Delete <title>") is preserved. Rows
// in the Archived section keep their inline Unarchive/Delete controls.
//
// These journeys involve no sandbox tool calls (the echo is a plain text turn),
// so they are fast and deterministic.

const conversationRegion = (page: Page) => page.getByRole("region", { name: /conversation/i });
const sidebar = (page: Page) => page.locator("aside").first();

// openRowMenu hovers a conversation row and opens its kebab menu, returning when
// the menu is on screen so a menuitem can be clicked.
async function openRowMenu(page: Page, row: Locator, title: string) {
  await row.hover();
  await row.getByRole("button", { name: `Conversation options for ${title}` }).click();
  await expect(page.getByRole("menu", { name: `Options for ${title}` })).toBeVisible();
}

// seedChat opens a fresh composer (after the first chat) and sends an echo turn
// whose reply — and thus the conversation title — is exactly `title`.
async function seedChat(page: Page, title: string, opts: { fresh?: boolean } = {}) {
  if (opts.fresh) {
    await page.getByRole("button", { name: "New chat" }).click();
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible({
      timeout: 10_000,
    });
  }
  const composer = page.getByPlaceholder(/message .* ai/i);
  await composer.fill(`[[echo:${title}]]`);
  await composer.press("Enter");
  // The assistant reply (== title) lands; gate on it so the turn has completed
  // and the conversation has been titled before we touch the sidebar.
  await expect(conversationRegion(page).getByText(title, { exact: true }).first()).toBeVisible({
    timeout: 60_000,
  });
  await expect(sidebar(page).getByText(title, { exact: true }).first()).toBeVisible({
    timeout: 10_000,
  });
}

test.describe("live conversation management (real chat server + Postgres)", () => {
  test("pin persists across reload and pinned conversations sort first", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Keep this chat"); // older
    await seedChat(page, "Delete later", { fresh: true }); // newer → sorts first by default

    const bar = sidebar(page);
    await expect(bar.getByText("Keep this chat", { exact: true })).toBeVisible();
    await expect(bar.getByText("Delete later", { exact: true })).toBeVisible();

    // Pin the older row from its kebab menu. Wait for the real pin POST to
    // confirm persistence happened (not just an optimistic UI).
    const keepRow = bar.locator("div.group").filter({ hasText: "Keep this chat" }).first();
    await openRowMenu(page, keepRow, "Keep this chat");
    const pinResponse = page.waitForResponse(
      (res) => /\/api\/conversations\/[^/]+\/pin$/.test(res.url()) && res.request().method() === "POST",
    );
    await page.getByRole("menuitem", { name: "Pin", exact: true }).click();
    expect((await pinResponse).status()).toBe(200);

    // The server is the source of truth: List must report it pinned.
    const origin = new URL(page.url()).origin;
    const convs = (await (
      await page.request.get("/api/conversations", { headers: { Origin: origin } })
    ).json()) as { conversations: Array<{ title: string; pinned: boolean }> };
    expect(convs.conversations.find((c) => c.title === "Keep this chat")?.pinned).toBe(true);

    // Reload from the server and verify the pinned chat now sorts first even
    // though it is the older one.
    await page.reload();
    const reloaded = sidebar(page);
    await expect(reloaded.getByText("Keep this chat", { exact: true })).toBeVisible({ timeout: 10_000 });
    await expect(reloaded.getByText("Delete later", { exact: true })).toBeVisible();
    const text = await reloaded.innerText();
    expect(text.indexOf("Keep this chat")).toBeLessThan(text.indexOf("Delete later"));
  });

  test("deleting a conversation removes it from the sidebar and the server", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Doomed chat");

    const bar = sidebar(page);
    const row = bar.locator("div.group").filter({ hasText: "Doomed chat" }).first();
    await openRowMenu(page, row, "Doomed chat");
    await page.getByRole("menuitem", { name: "Delete", exact: true }).click();
    // Confirm in the modal.
    await page.getByRole("button", { name: /^delete$/i }).click();

    await expect(bar.getByText("Doomed chat", { exact: true })).toHaveCount(0, { timeout: 10_000 });

    // Confirm server-side too.
    const origin = new URL(page.url()).origin;
    const convs = (await (
      await page.request.get("/api/conversations", { headers: { Origin: origin } })
    ).json()) as { conversations: Array<{ title: string }> | null };
    expect((convs.conversations ?? []).some((c) => c.title === "Doomed chat")).toBe(false);
  });

  test("'Delete all unpinned' leaves pinned conversations untouched", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Pinned survivor");
    await seedChat(page, "Unpinned victim", { fresh: true });

    const bar = sidebar(page);
    const keepRow = bar.locator("div.group").filter({ hasText: "Pinned survivor" }).first();
    await openRowMenu(page, keepRow, "Pinned survivor");
    const pinResponse = page.waitForResponse(
      (res) => /\/api\/conversations\/[^/]+\/pin$/.test(res.url()) && res.request().method() === "POST",
    );
    await page.getByRole("menuitem", { name: "Pin", exact: true }).click();
    expect((await pinResponse).status()).toBe(200);

    await page.getByRole("button", { name: /delete all unpinned/i }).click();
    await page.getByRole("button", { name: /^delete all$/i }).click();

    await expect(bar.getByText("Unpinned victim", { exact: true })).toHaveCount(0, { timeout: 10_000 });
    await expect(bar.getByText("Pinned survivor", { exact: true })).toBeVisible();
  });

  test("archiving hides a chat into the Archived section and unarchiving restores it", async ({
    page,
    login,
  }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Filed away chat");

    const bar = sidebar(page);
    const row = bar.locator("div.group").filter({ hasText: "Filed away chat" }).first();
    await openRowMenu(page, row, "Filed away chat");
    const archiveResponse = page.waitForResponse(
      (res) =>
        /\/api\/conversations\/[^/]+\/archive$/.test(res.url()) && res.request().method() === "POST",
    );
    await page.getByRole("menuitem", { name: "Archive", exact: true }).click();
    expect((await archiveResponse).status()).toBe(200);

    // Gone from the main list; surfaced under a collapsed "Archived" section.
    // Match the toggle count-agnostically (the badge count is shared state).
    const archivedToggle = bar.getByRole("button", { name: /Archived conversations/i });
    await expect(archivedToggle).toBeVisible({ timeout: 10_000 });

    // Server is the source of truth: it reports the chat as archived.
    const origin = new URL(page.url()).origin;
    const archived = (await (
      await page.request.get("/api/conversations?archived=true", { headers: { Origin: origin } })
    ).json()) as { conversations: Array<{ title: string; archived_at: number | null }> | null };
    const found = (archived.conversations ?? []).find((c) => c.title === "Filed away chat");
    expect(found?.archived_at).toBeTruthy();

    // Expand the section and unarchive the specific row (Archived rows keep their
    // inline Unarchive/Delete controls).
    await archivedToggle.click();
    const archivedRow = bar.locator("div.group").filter({ hasText: "Filed away chat" }).first();
    await expect(archivedRow).toBeVisible({ timeout: 10_000 });
    await archivedRow.hover();
    const unarchiveResponse = page.waitForResponse(
      (res) =>
        /\/api\/conversations\/[^/]+\/archive$/.test(res.url()) && res.request().method() === "POST",
    );
    await archivedRow.getByRole("button", { name: "Unarchive Filed away chat" }).click({ force: true });
    expect((await unarchiveResponse).status()).toBe(200);

    // Back in the main list; no longer reported as archived by the server.
    await expect(archivedToggle).toHaveCount(0, { timeout: 10_000 });
    await expect(bar.getByText("Filed away chat", { exact: true })).toBeVisible();
    const after = (await (
      await page.request.get("/api/conversations?archived=true", { headers: { Origin: origin } })
    ).json()) as { conversations: Array<{ title: string }> | null };
    expect((after.conversations ?? []).some((c) => c.title === "Filed away chat")).toBe(false);
  });

  test("deleting a chat from the Archived section removes it everywhere", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Archived doomed");

    const bar = sidebar(page);
    const row = bar.locator("div.group").filter({ hasText: "Archived doomed" }).first();
    await openRowMenu(page, row, "Archived doomed");
    const archiveResponse = page.waitForResponse(
      (res) =>
        /\/api\/conversations\/[^/]+\/archive$/.test(res.url()) && res.request().method() === "POST",
    );
    await page.getByRole("menuitem", { name: "Archive", exact: true }).click();
    expect((await archiveResponse).status()).toBe(200);

    // Expand the Archived section, then delete from it (the ghost-row regression).
    await bar.getByRole("button", { name: /Archived conversations/i }).click();
    const archivedRow = bar.locator("div.group").filter({ hasText: "Archived doomed" }).first();
    await expect(archivedRow).toBeVisible({ timeout: 10_000 });
    await archivedRow.hover();
    await archivedRow.getByRole("button", { name: "Delete Archived doomed" }).click({ force: true });
    await page.getByRole("button", { name: /^delete$/i }).click();

    // The row must be gone from the UI (no ghost) and from the server.
    await expect(bar.getByText("Archived doomed", { exact: true })).toHaveCount(0, { timeout: 10_000 });
    const origin = new URL(page.url()).origin;
    const archived = (await (
      await page.request.get("/api/conversations?archived=true", { headers: { Origin: origin } })
    ).json()) as { conversations: Array<{ title: string }> | null };
    expect((archived.conversations ?? []).some((c) => c.title === "Archived doomed")).toBe(false);
  });

  test("reloading mid-conversation resumes full history from the chat server", async ({ page, login }) => {
    test.setTimeout(120_000);
    await login();

    await seedChat(page, "Resume me");
    await page.reload();

    // History is replayed from the server; the assistant reply must re-render.
    await expect(conversationRegion(page).getByText("Resume me", { exact: true }).first()).toBeVisible({
      timeout: 15_000,
    });
    // And it must not be a mock/short-circuited backend.
    await expect(conversationRegion(page)).not.toContainText(/Mock reply to:/i);
  });
});
