import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the headline flow of ADR-0057, end to end through the UI:
//
//   share a chat with the team from the Share dialog  →
//   a teammate finds it in the project home's Team section  →
//   opens it read-only  →
//   branches it into a chat of their own.
//
// It also pins the narrowing that makes the feature coherent: a chat can be
// shared with the team only inside a TEAM-SHARED project, and the dialog's
// team block names the situation (A1 / A2 / A4) and its one fix when it is not.

const SHARED_PROJECT = {
  id: "p-quant",
  owner_email: "e2e@example.com",
  name: "Quant",
  instructions: "",
  team_id: "quant",
  mcp_servers: [],
  created_at: 1_700_000_000,
  updated_at: 1_700_000_500,
};

const PERSONAL_PROJECT = {
  ...SHARED_PROJECT,
  id: "p-mine",
  name: "Mine",
  team_id: undefined,
};

async function mockProjects(page: Page, projects: unknown[]) {
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects } }));
  await page.route("**/api/me/team", (r: Route) =>
    r.fulfill({
      json: { email: "e2e@example.com", role: "member", team_id: "quant", admin: false },
    }),
  );
  // The project home's own reads; each spec overrides what it cares about.
  await page.route("**/api/projects/*/files{,?*}", (r: Route) =>
    r.fulfill({ json: { groups: [], files: [], truncated: false } }),
  );
  // Per-person project state: past the getting-started card.
  await page.route("**/api/projects/*/my-state", (r: Route) =>
    r.fulfill({ json: { kept_personal: false, has_shared_chat: true, sources_open: {} } }),
  );
  await page.route("**/api/projects/*/memories", (r: Route) =>
    r.fulfill({ json: { memories: [] } }),
  );
  await page.route("**/api/projects/*/team-conversations", (r: Route) =>
    r.fulfill({ json: { conversations: [] } }),
  );
  await page.route("**/api/projects/*/conversations", (r: Route) =>
    r.fulfill({ json: { conversations: [] } }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("the Share dialog holds both audiences and only offers the team inside a team-shared project", async ({
  page,
}) => {
  await mockProjects(page, [SHARED_PROJECT, PERSONAL_PROJECT]);
  await mockChatBoot(page, {
    conversations: [
      { id: "c-shared", title: "In Quant", project_id: "p-quant" },
      { id: "c-personal", title: "In Mine", project_id: "p-mine" },
      { id: "c-loose", title: "Filed nowhere" },
    ],
  });

  const teamWrites: string[] = [];
  await page.route("**/api/conversations/c-shared/share-with-team", async (r: Route) => {
    teamWrites.push(r.request().postData() ?? "");
    await r.fulfill({ json: { team_visible: true, shared_files: 2, total_files: 2 } });
  });
  // The chat's outputs: what the A4 file line counts and the toast quotes.
  await page.route("**/api/conversations/*/outputs", (r: Route) =>
    r.fulfill({
      json: {
        outputs: [
          { path: "out/report.xlsx", name: "report.xlsx", size: 2048, modified_at: 1_700_000_300, shared: true },
          { path: "out/chart.png", name: "chart.png", size: 4096, modified_at: 1_700_000_200, shared: true },
        ],
        total: 2,
        shared_count: 2,
        team_visible: false,
      },
    }),
  );

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const rail = page.locator("aside").first();
  // Project chats live under their project in the rail, collapsed by default.
  const expandProject = async (name: string, chats: number) => {
    await rail
      .getByRole("button", { name: `Project ${name} (${chats} chats)` })
      .click();
  };
  const openShare = async (title: string) => {
    const menu = page.getByRole("menu", { name: `Options for ${title}` });
    await expect(async () => {
      await rail.getByRole("button", { name: `Conversation options for ${title}` }).click();
      await expect(menu).toBeVisible({ timeout: 1500 });
    }).toPass({ timeout: 15_000 });
    await menu.getByRole("menuitem", { name: "Share…", exact: true }).click();
    return page.getByRole("dialog", { name: "Share this chat" });
  };
  const teamBlock = (dialog: ReturnType<typeof page.getByRole>) =>
    dialog.getByRole("region", { name: "Share with your team" });

  // A1 — a chat filed nowhere: it cannot be shared where it is, so the team
  // block says why and offers the one fix (move it into a project shared with
  // the team). Only team-shared projects are offered as the destination.
  let dialog = await openShare("Filed nowhere");
  await expect(dialog.getByTestId("share-dialog-where")).toContainText("Not in a project");
  let block = teamBlock(dialog);
  await expect(block).toHaveAttribute("data-state", "A1");
  await expect(block).toContainText(
    "To share a chat with quant, it needs to be in a project shared with quant. Pick one and this chat will move into it.",
  );
  await expect(block.getByLabel("Project shared with quant").locator("option")).toHaveText(["Quant"]);
  await expect(block.getByRole("button", { name: "Move and share" })).toBeEnabled();
  // Opening the dialog must not mint a public link as a side effect.
  await expect(dialog.getByLabel("Share link URL")).toHaveCount(0);
  await dialog.getByRole("button", { name: "Close share dialog" }).click();

  // A2 — a chat in a PERSONAL project: a different situation, a different
  // sentence, and the fix is sharing the project first — not this chat.
  await expandProject("Mine", 1);
  dialog = await openShare("In Mine");
  block = teamBlock(dialog);
  await expect(block).toHaveAttribute("data-state", "A2");
  await expect(block).toContainText("Mine isn’t shared with quant, so its chats are private to you.");
  await expect(block.getByRole("button", { name: "Share project first" })).toBeVisible();
  await expect(block.getByRole("button", { name: "Share with quant", exact: true })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Close share dialog" }).click();

  // A4 → A5 — a chat in a team-shared project: one button shares it (with
  // its files), names the team, and the dialog moves to the shared state.
  await expandProject("Quant", 1);
  dialog = await openShare("In Quant");
  block = teamBlock(dialog);
  await expect(block).toHaveAttribute("data-state", "A4");
  await expect(block.getByTestId("share-file-line")).toContainText("Includes 2 files");
  await block.getByRole("button", { name: "Share with quant", exact: true }).click();
  // The checklist was never opened, so the owner's file choices are untouched.
  await expect.poll(() => teamWrites.map((w) => JSON.parse(w))).toEqual([
    { visible: true },
  ]);
  await expect(page.getByText("“In Quant” is shared with quant, with 2 files.")).toBeVisible();
  await expect(block.getByRole("button", { name: "Copy link for quant" })).toBeVisible();
  await dialog.getByRole("button", { name: "Close share dialog" }).click();

  // The row now carries the TEAM badge, distinct from the link badge and
  // labelled with its audience.
  await expect(rail.getByLabel("Shared with your team")).toBeVisible();
});

test("a teammate finds a shared chat on the project home, reads it, and branches it", async ({
  page,
}) => {
  await mockProjects(page, [SHARED_PROJECT]);
  await page.route("**/api/projects/p-quant/team-conversations", (r: Route) =>
    r.fulfill({
      json: {
        conversations: [
          {
            id: "c-theirs",
            title: "Basis trade",
            user_email: "bob@example.com",
            updated_at: 1_700_000_400,
          },
        ],
      },
    }),
  );
  await page.route("**/api/conversations/c-theirs/team-view", (r: Route) =>
    r.fulfill({
      json: {
        id: "c-theirs",
        owner_email: "bob@example.com",
        title: "Basis trade",
        persona: "default",
        model: "test-model",
        team_id: "quant",
        project_id: "p-quant",
        created_at: 1_700_000_000,
        updated_at: 1_700_000_400,
        messages: [
          { id: 11, role: "user", type: "text", content: { text: "how did you size it?" } },
          { id: 12, role: "assistant", type: "text", content: { text: "half a turn of carry" } },
        ],
      },
    }),
  );
  const branchBodies: string[] = [];
  await page.route("**/api/conversations/c-theirs/branch", async (r: Route) => {
    branchBodies.push(r.request().postData() ?? "");
    await r.fulfill({
      status: 201,
      json: { id: "c-fork", title: "Basis trade (from bob)", project_id: "p-quant" },
    });
  });

  await mockChatBoot(page, { conversations: [] });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  await page.getByRole("button", { name: "Open project Quant" }).click();
  const home = page.getByTestId("project-home");
  await expect(home).toBeVisible();

  // "Shared by your team" names whose chat it is and what a reader can do.
  const row = home.getByTestId("team-chat-row");
  await expect(row).toContainText("bob@example.com");
  await expect(row).toContainText("Read, branch");
  await row.click();

  // The read-only viewer (B19): whose it is, and where the composer would be,
  // a read-only line — but not a dead end either.
  const viewer = page.getByTestId("team-chat-viewer");
  await expect(viewer).toBeVisible();
  await expect(viewer.getByTestId("team-view-shared-by")).toHaveText("Shared by bob@example.com");
  await expect(viewer.getByText("Read-only. This is Bob’s chat.")).toBeVisible();
  await expect(viewer.getByRole("textbox")).toHaveCount(0);
  await expect(viewer.getByText("how did you size it?")).toBeVisible();

  await viewer.getByRole("button", { name: /Branch to continue in your own chat/ }).click();
  await expect.poll(() => branchBodies.map((b) => JSON.parse(b).branch_point_message_id)).toEqual([12]);
  // The reader lands in their own chat, not back on the read-only view.
  await expect(page.getByTestId("team-chat-viewer")).toHaveCount(0);
});

// The viewer is a full-page overlay: while it is up, the chat pane is hidden
// entirely. It was cleared only by its own Back arrow and by Branch, so any
// other navigation — a rail row, New chat — loaded a conversation invisibly
// behind it and the app simply looked frozen.
test("leaving the team-chat viewer by any route, not just its back arrow", async ({
  page,
}) => {
  await mockProjects(page, [SHARED_PROJECT]);
  await page.route("**/api/projects/p-quant/team-conversations", (r: Route) =>
    r.fulfill({
      json: {
        conversations: [
          {
            id: "c-theirs",
            title: "Basis trade",
            user_email: "bob@example.com",
            updated_at: 1_700_000_400,
          },
        ],
      },
    }),
  );
  await page.route("**/api/conversations/c-theirs/team-view", (r: Route) =>
    r.fulfill({
      json: {
        id: "c-theirs",
        owner_email: "bob@example.com",
        title: "Basis trade",
        team_id: "quant",
        updated_at: 1_700_000_400,
        messages: [
          { id: 11, role: "user", type: "text", content: { text: "how did you size it?" } },
        ],
      },
    }),
  );
  await mockChatBoot(page, {
    conversations: [
      {
        id: "c-mine",
        title: "My own chat",
        persona: "default",
        model: "test-model",
        pinned: false,
        updated_at: 1_700_000_500,
      },
    ],
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });

  const openViewer = async () => {
    await page.getByRole("button", { name: "Open project Quant" }).click();
    await page.getByTestId("project-home").getByRole("button", { name: /Basis trade/ }).click();
    await expect(page.getByTestId("team-chat-viewer")).toBeVisible();
  };

  // 1. A rail row.
  await openViewer();
  await page.getByRole("complementary").getByText("My own chat").click();
  await expect(page.getByTestId("team-chat-viewer")).toHaveCount(0);

  // 2. New chat.
  await openViewer();
  await page.getByRole("button", { name: /New chat/i }).first().click();
  await expect(page.getByTestId("team-chat-viewer")).toHaveCount(0);
});
