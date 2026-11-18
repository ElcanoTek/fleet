import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the unified rail search (#308, merged from the old Cmd/Ctrl+K
// palette): Cmd/Ctrl+K focuses the rail's search bar, a debounced query renders
// full-text "In messages" hits with <mark> highlights alongside the client-side
// title filter, and clicking a hit loads that conversation. /api/search is
// intercepted so the suite is deterministic (no Go chat-server).

async function mockSearch(page: Page) {
  await page.route("**/api/search**", (r: Route) =>
    r.fulfill({
      json: {
        results: [
          {
            conversation_id: "conv-search-1",
            title: "Python async patterns",
            match_preview: "the async <mark>function</mark> you asked about",
            matched_at: 1719432000,
          },
        ],
        total: 1,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("Ctrl+K focuses the rail search; a query shows highlighted message hits", async ({
  page,
}) => {
  await mockChatBoot(page);
  await mockSearch(page);
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page.keyboard.press("Control+k");
  await expect(page.getByTestId("search-input")).toBeFocused();

  await page.getByTestId("search-input").fill("python");
  const result = page.getByTestId("search-result").first();
  await expect(result).toBeVisible({ timeout: 5_000 });
  await expect(result).toContainText("Python async patterns");
  // The preview's matched term renders as a real <mark> element (sanitized HTML).
  await expect(result.locator("mark")).toHaveText("function");

  // Clearing the query returns the rail to its sectioned view.
  await page.getByTestId("search-input").fill("");
  await expect(page.getByTestId("search-result")).toHaveCount(0);
});

test("a query filters the Projects section and breadcrumbs message hits", async ({ page }) => {
  await page.route("**/api/projects", (r: Route) =>
    r.fulfill({
      json: {
        projects: [
          {
            id: "p-python",
            owner_email: "e2e@example.com",
            name: "Python migrations",
            mcp_servers: [],
            created_at: 1719432000,
            updated_at: 1719432000,
          },
          {
            id: "p-ops",
            owner_email: "e2e@example.com",
            name: "Ops runbooks",
            mcp_servers: [],
            created_at: 1719432000,
            updated_at: 1719432001,
          },
        ],
      },
    }),
  );
  await mockChatBoot(page, {
    // The message hit's conversation lives in the matching project, so the
    // result row shows the "Project › title" path.
    conversations: [{ id: "conv-search-1", title: "Deploy notes", project_id: "p-python" }],
  });
  await page.route("**/api/search**", (r: Route) =>
    r.fulfill({
      json: {
        results: [
          {
            conversation_id: "conv-search-1",
            title: "Deploy notes",
            match_preview: "the async <mark>python</mark> job",
            matched_at: 1719432000,
          },
        ],
        total: 1,
      },
    }),
  );
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page.getByTestId("search-input").fill("python");
  // The matching project appears as a real tree row in the results' Projects
  // group (split row: expand chevron + open-home name button); the
  // non-matching one is filtered out.
  await expect(
    page.getByRole("button", { name: "Open project Python migrations" }),
  ).toBeVisible({ timeout: 5_000 });
  await expect(
    page.getByRole("button", { name: "Open project Ops runbooks" }),
  ).toBeHidden();
  // The message hit carries its path: Project › chat title, snippet below.
  const hit = page.getByTestId("search-result").first();
  await expect(hit).toContainText("Python migrations ›");
  await expect(hit).toContainText("Deploy notes");
  await expect(hit.locator("mark")).toHaveText("python");
});

test("clicking a message hit loads that conversation", async ({ page }) => {
  await mockChatBoot(page);
  await mockSearch(page);
  // The result click calls loadConversation → GET /api/conversations/<id>.
  await page.route("**/api/conversations/conv-search-1", (r: Route) =>
    r.fulfill({
      json: {
        conversation: {
          id: "conv-search-1",
          title: "Python async patterns",
          persona: "default",
          model: "",
          pinned: false,
          created_at: 1719432000,
          updated_at: 1719432000,
        },
        history: [
          {
            role: "user",
            type: "text",
            content: { text: "tell me about async functions" },
          },
        ],
      },
    }),
  );
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page.keyboard.press("Control+k");
  await page.getByTestId("search-input").fill("python");
  await page.getByTestId("search-result").first().click();

  // The chosen conversation's content renders (scope to the conversation
  // body — the same text also appears in the title-rename button).
  await expect(
    page
      .getByLabel("Conversation", { exact: true })
      .getByText("tell me about async functions"),
  ).toBeVisible({ timeout: 10_000 });
});

// Search looks everywhere, whatever the sidebar is set to show
// (docs/SIDEBAR-SEARCH.md). The report: with Team chats folded, a search for a
// teammate's chat showed nothing at all, and the chats looked deleted.
const SEARCH_CHATS = [
  { id: "c1", title: "Budget pacing review" },
  { id: "t1", title: "Quarterly pacing plan", team_visible: true, user_email: "dana@example.com" },
  { id: "m1", title: "Launch checklist", multiplayer: true, user_email: "lee@example.com" },
];

// `separate` is the layout of a person who chose separate lists (Team chats
// as a section of its own, with its own fold); the default is one list.
async function mockSearchRail(page: Page, sections: Record<string, unknown>[], separate = false) {
  const puts: Record<string, unknown>[] = [];
  await mockChatBoot(page, { conversations: SEARCH_CHATS });
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [] } }));
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: [] } }));
  await page.route("**/api/search**", (r: Route) => r.fulfill({ json: { results: [], total: 0 } }));
  let own: Record<string, unknown> | null = { version: 1, items: [], sections, ...(separate ? { separate_lists: true } : {}) };
  await page.route("**/api/sidebar", (r: Route) => {
    if (r.request().method() === "PUT") {
      own = r.request().postDataJSON() as Record<string, unknown>;
      puts.push(own);
      return r.fulfill({ json: { layout: own } });
    }
    return r.fulfill({ json: { layout: own, workspace_default: null } });
  });
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  return { puts };
}

test("a search finds a teammate's chat while every section is folded", async ({ page }) => {
  await mockSearchRail(page, [
    { id: "team", collapsed: true },
    { id: "projects", collapsed: true },
    { id: "chats", collapsed: true },
  ], true);
  // Folded: the rail lists no chats at all.
  await expect(page.getByTestId("team-shared-conv-row")).toHaveCount(0);

  await page.getByTestId("search-input").fill("pacing plan");
  const team = page.getByTestId("search-group-team");
  await expect(team.getByTestId("team-shared-conv-row")).toHaveCount(1);
  await expect(team.getByTestId("team-shared-conv-row")).toContainText("Quarterly pacing plan");
  await expect(team.locator("mark")).toHaveText(["pacing", "plan"]);
  await expect(page.getByTestId("search-hidden-note")).toHaveCount(0);

  // The keyboard reaches a result from the field, and gets back to it.
  await page.getByTestId("search-input").press("ArrowDown");
  await expect(page.getByTestId("search-results").locator("[data-rail-row]").first()).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(page.getByTestId("search-input")).toBeFocused();

  const results = await new AxeBuilder({ page }).include('[data-testid="search-results"]').analyze();
  expect(results.violations).toEqual([]);

  // Clearing the search gives the folded rail back.
  await page.getByRole("button", { name: "Clear search" }).click();
  await expect(page.getByTestId("search-results")).toHaveCount(0);
  await expect(page.getByTestId("team-chats-section")).toBeVisible();
});

test("a search finds chats in a hidden section, says so, and can bring it back", async ({ page }) => {
  const { puts } = await mockSearchRail(page, [
    { id: "projects" },
    { id: "chats" },
    { id: "team", hidden: true },
  ], true);
  await expect(page.getByTestId("team-chats-section")).toHaveCount(0);

  // By whose chat it is, too.
  await page.getByTestId("search-input").fill("lee");
  const team = page.getByTestId("search-group-team");
  await expect(team.getByTestId("team-shared-conv-row")).toContainText("Launch checklist");
  await expect(team.getByTestId("search-hidden-note")).toBeVisible();

  await team.getByRole("button", { name: "Show Team chats in the sidebar" }).click();
  await expect(team.getByTestId("search-hidden-note")).toHaveCount(0);
  // The choice is saved to the account, with only that section changed.
  await expect.poll(() => puts.length).toBeGreaterThan(0);
  const saved = puts.at(-1)?.sections as { id: string; hidden?: boolean }[];
  expect(saved.find((x) => x.id === "team")?.hidden).toBeFalsy();

  await page.getByTestId("search-input").fill("");
  await expect(page.getByTestId("team-chats-section")).toBeVisible();
});

test("a search with no match says where it looked and offers to clear", async ({ page }) => {
  await mockSearchRail(page, [{ id: "projects" }, { id: "chats" }, { id: "team" }]);
  await page.getByTestId("search-input").fill("zebra");
  const empty = page.getByTestId("search-empty");
  await expect(empty).toContainText("Nothing matches “zebra”.");
  await expect(empty).toContainText("including hidden and folded sections");
  await empty.getByRole("button", { name: "Clear search" }).click();
  await expect(page.getByTestId("search-input")).toHaveValue("");
  await expect(page.getByTestId("search-input")).toBeFocused();
});

test("with one list (the default), a folded Chats list never hides a search result", async ({ page }) => {
  await mockSearchRail(page, [{ id: "projects" }, { id: "chats", collapsed: true }, { id: "team" }]);
  // Folded: own and teammates' chats are out of sight together.
  await expect(page.getByTestId("unified-chat-list")).toBeHidden();
  await page.getByTestId("search-input").fill("checklist");
  await expect(page.getByTestId("search-group-team").getByTestId("team-shared-conv-row")).toContainText("Launch checklist");
  await expect(page.getByTestId("search-hidden-note")).toHaveCount(0);
});
