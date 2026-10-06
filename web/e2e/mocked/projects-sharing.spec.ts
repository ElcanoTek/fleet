import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for Fleet Projects sharing's happy path (the spec's acceptance
// checklist, canvas section C), from both sides of the team:
//
//   owner:    the row pill on the project home shares a chat → the B5 toast
//             names the file count → its "Manage" opens Sources at THAT
//             chat's group (#36 / #05);
//   teammate: "Shared by your team" → the read-only view (B19) with the
//             shared output as a live download, the unshared one as a locked
//             name, and Branch where the composer would be.
//
// Every backend read the two pages make is mocked here, so the spec pins the
// UI's use of the new endpoints (outputs, grouped project files, my-state,
// team-view files) rather than letting them 502 against an absent Go server.

const ME = "e2e@example.com";

type Group = Record<string, unknown>;

async function mockProject(
  page: Page,
  opts: {
    project: Record<string, unknown>;
    myChats: Array<Record<string, unknown>>;
    teamChats?: Array<Record<string, unknown>>;
    groups: () => Group[];
  },
) {
  await page.route("**/api/projects", (r: Route) =>
    r.fulfill({ json: { projects: [opts.project] } }),
  );
  await page.route("**/api/me/team", (r: Route) =>
    r.fulfill({ json: { email: ME, role: "member", team_id: "quant", admin: false } }),
  );
  await page.route("**/api/projects/*/conversations", (r: Route) =>
    r.fulfill({ json: { conversations: opts.myChats } }),
  );
  await page.route("**/api/projects/*/team-conversations", (r: Route) =>
    r.fulfill({ json: { conversations: opts.teamChats ?? [] } }),
  );
  await page.route("**/api/projects/*/memories", (r: Route) =>
    r.fulfill({ json: { memories: [] } }),
  );
  await page.route("**/api/projects/*/members", (r: Route) =>
    r.fulfill({ json: { members: [ME, "sam@example.com"] } }),
  );
  // Past the getting-started card, no Sources groups remembered open — so
  // the panel's own default (the most recently active group) applies.
  await page.route("**/api/projects/*/my-state", (r: Route) =>
    r.fulfill({ json: { kept_personal: false, has_shared_chat: true, sources_open: {} } }),
  );
  await page.route("**/api/projects/*/files", (r: Route) =>
    r.fulfill({ json: { groups: opts.groups(), files: [], truncated: false } }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("owner shares a chat from its row pill; the toast counts the files and Manage opens Sources at that chat", async ({
  page,
}) => {
  const project = {
    id: "p-quant",
    owner_email: ME,
    name: "Quant",
    instructions: "",
    team_id: "quant",
    mcp_servers: [],
    created_at: 1_700_000_000,
    updated_at: 1_700_000_500,
  };
  let recapShared = false;
  const recapFiles = [
    { path: "out/report.xlsx", name: "report.xlsx", size: 2048, modified_at: 1_700_000_300 },
    { path: "out/chart.png", name: "chart.png", size: 4096, modified_at: 1_700_000_200 },
  ];
  const groups = (): Group[] => [
    // The most recently active chat: open by default.
    {
      conversation_id: "c-vol",
      title: "Vol study",
      owner_email: ME,
      mine: true,
      team_visible: false,
      is_branch: false,
      last_active_at: 1_700_000_900,
      file_count: 1,
      shared_count: 1,
      files: [
        { path: "vol.csv", name: "vol.csv", size: 512, modified_at: 1_700_000_800, shared: true, output: true, your_copy: false },
      ],
    },
    {
      conversation_id: "c-recap",
      title: "Q3 recap",
      owner_email: ME,
      mine: true,
      team_visible: recapShared,
      is_branch: false,
      last_active_at: 1_700_000_400,
      file_count: 2,
      shared_count: 2,
      files: recapFiles.map((f) => ({ ...f, shared: true, output: true, your_copy: false })),
    },
  ];
  await mockProject(page, {
    project,
    myChats: [
      { id: "c-vol", title: "Vol study", updated_at: 1_700_000_900, team_visible: false },
      { id: "c-recap", title: "Q3 recap", updated_at: 1_700_000_400, team_visible: false },
    ],
    groups,
  });
  const shareWrites: Array<{ url: string; body: unknown }> = [];
  await page.route("**/api/conversations/*/share-with-team", async (r: Route) => {
    shareWrites.push({ url: r.request().url(), body: JSON.parse(r.request().postData() ?? "{}") });
    recapShared = true;
    await r.fulfill({ json: { team_visible: true, shared_files: 2, total_files: 2 } });
  });
  await mockChatBoot(page, {
    conversations: [
      { id: "c-vol", title: "Vol study", project_id: "p-quant" },
      { id: "c-recap", title: "Q3 recap", project_id: "p-quant" },
    ],
  });

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "Open project Quant" }).click();
  const home = page.getByTestId("project-home");
  await expect(home).toBeVisible();

  // Sources before: the most recently active chat's group is open, Q3
  // recap's is closed.
  const sources = home.getByTestId("project-sources");
  const recapGroup = sources.locator('[data-testid="sources-group"][data-conversation-id="c-recap"]');
  const recapHeader = recapGroup.getByRole("button").first();
  await expect(recapHeader).toHaveAttribute("aria-expanded", "false");
  await expect(
    sources.locator('[data-testid="sources-group"][data-conversation-id="c-vol"]').getByRole("button").first(),
  ).toHaveAttribute("aria-expanded", "true");

  // The row pill: Only you → quant, straight from the row (a fast path:
  // every output goes with it, no checklist).
  const row = home.getByTestId("project-chat-row").filter({ hasText: "Q3 recap" });
  await row.getByRole("button", { name: "Who can see this chat: Only you" }).click();
  await page.getByRole("menu", { name: "Who can see this chat" }).getByRole("menuitem", { name: /^quant/ }).click();

  await expect.poll(() => shareWrites).toEqual([
    { url: expect.stringContaining("/api/conversations/c-recap/share-with-team"), body: { visible: true } },
  ]);
  await expect(row.getByRole("button", { name: "Who can see this chat: quant" })).toBeVisible();

  // B5: the toast names the chat, the team and the file count, with Manage.
  const toast = page.getByText("“Q3 recap” is shared with quant, with 2 files.");
  await expect(toast).toBeVisible();
  await page.getByRole("button", { name: "Manage", exact: true }).click();

  // Manage opens Sources at THIS chat's group: open, in view, with its files
  // marked shared.
  await expect(recapHeader).toHaveAttribute("aria-expanded", "true");
  await expect(recapGroup).toBeInViewport();
  await expect(recapGroup.getByTestId("sources-file")).toHaveCount(2);
  await expect(recapGroup.getByTestId("sources-file").first()).toContainText("report.xlsx");
  await expect(recapGroup.getByTestId("sources-file").first()).toContainText("· Shared");
});

test("a teammate reads a shared chat: shared file downloads, unshared file is a locked name, Branch continues it", async ({
  page,
}) => {
  const project = {
    id: "p-quant",
    owner_email: "sam@example.com",
    name: "Quant",
    instructions: "",
    team_id: "quant",
    mcp_servers: [],
    created_at: 1_700_000_000,
    updated_at: 1_700_000_500,
  };
  await mockProject(page, {
    project,
    myChats: [],
    teamChats: [
      {
        id: "c-sam",
        title: "Q3 recap",
        user_email: "sam@example.com",
        updated_at: 1_700_000_400,
        viewer_branch: null,
      },
    ],
    groups: () => [
      {
        conversation_id: "c-sam",
        title: "Q3 recap",
        owner_email: "sam@example.com",
        mine: false,
        team_visible: true,
        is_branch: false,
        last_active_at: 1_700_000_400,
        file_count: 1,
        shared_count: 1,
        files: [
          { path: "out/report.xlsx", name: "report.xlsx", size: 2048, modified_at: 1_700_000_300, shared: true, output: true, your_copy: false },
        ],
      },
    ],
  });
  await page.route("**/api/conversations/c-sam/team-view", (r: Route) =>
    r.fulfill({
      json: {
        id: "c-sam",
        owner_email: "sam@example.com",
        title: "Q3 recap",
        team_id: "quant",
        project_id: "p-quant",
        project_name: "Quant",
        updated_at: 1_700_000_400,
        viewer_branch: null,
        files: [
          { path: "out/report.xlsx", name: "report.xlsx", size: 2048, modified_at: 1_700_000_300, shared: true },
          { path: "out/exclusion_list_v1.json", name: "exclusion_list_v1.json", size: 300, modified_at: 1_700_000_200, shared: false },
        ],
        messages: [
          { id: 21, role: "user", type: "text", content: { text: "Summarise Q3 for the team." } },
          {
            id: 22,
            role: "assistant",
            type: "text",
            content: {
              text: "Here is [the Q3 report](out/report.xlsx). The working list is [exclusion_list_v1.json](out/exclusion_list_v1.json).",
            },
          },
        ],
      },
    }),
  );
  const branchBodies: unknown[] = [];
  await page.route("**/api/conversations/c-sam/branch", async (r: Route) => {
    branchBodies.push(JSON.parse(r.request().postData() ?? "{}"));
    await r.fulfill({
      status: 201,
      json: { id: "c-fork", title: "Q3 recap (branch)", project_id: "p-quant" },
    });
  });
  await mockChatBoot(page, { conversations: [] });

  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await page.getByRole("button", { name: "Open project Quant" }).click();
  const home = page.getByTestId("project-home");
  await expect(home).toBeVisible();

  // Sources, for a teammate: "From your team" with the owner named.
  const sources = home.getByTestId("project-sources");
  await expect(sources.getByText("From your team")).toBeVisible();
  await expect(sources.getByTestId("sources-group")).toContainText("Shared by sam@example.com");

  await home.getByTestId("team-chat-row").filter({ hasText: "Q3 recap" }).click();

  const viewer = page.getByTestId("team-chat-viewer");
  await expect(viewer).toBeVisible();
  await expect(viewer.getByTestId("team-view-shared-by")).toHaveText("Shared by sam@example.com");

  // The shared output is a live download through the team-files route…
  const download = viewer.getByRole("link", { name: "the Q3 report" });
  await expect(download).toHaveAttribute(
    "href",
    "/api/conversations/c-sam/team-files/out/report.xlsx",
  );
  // …the one the owner unchecked is a locked name, never a link to the file.
  await expect(viewer.getByText("exclusion_list_v1.json (not shared)")).toBeVisible();
  await expect(viewer.locator('a[href*="exclusion_list_v1.json"]')).toHaveCount(0);

  // Where the composer would be: the read-only line and Branch.
  await expect(viewer.getByText("Read-only. This is Sam’s chat.")).toBeVisible();
  await viewer.getByRole("button", { name: "Branch to continue in your own chat" }).click();
  await expect.poll(() => branchBodies.length).toBe(1);
  await expect(page.getByTestId("team-chat-viewer")).toHaveCount(0);
});
