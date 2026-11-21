import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The redesigned sidebar (docs/SIDEBAR.md), with a lot in it: about two
// hundred of the person's own chats, a dozen projects, thirty-odd teammates'
// chats and a handful of patrols. One sidebar that scrolls as a whole and is
// the same on every page; search, a pen and a + menu at its top; Patrols and
// Background tasks under them; then Pinned, Projects and Chats, with quiet
// rows (a title, and one status when it matters); a gear that opens a quick
// Customize panel; an edge that drags to resize; and a Patrols page listing
// the person's patrols and the ones shared with them. Checked in light and
// dark, on a desktop and on a phone. All /api/* calls are intercepted.

const ME = "e2e@example.com";
const DAY = 86_400;
// The fixtures are built when a test starts, never when this file is loaded:
// what a row says about time ("19h", "6d", Today) is read against the
// browser's clock, and one worker runs the whole file over several minutes.
const nowSeconds = () => Math.floor(Date.now() / 1000);

type Row = Record<string, unknown> & { id: string; title: string };

const PROJECTS = [
  ["p-aod", "Always on Data - Cohen Media", "team-1"],
  ["p-reklaim", "Reklaim", ""],
  ["p-pitches", "Pitches", ""],
  ["p-marketing", "Marketing", "team-1"],
  ["p-choozle", "Choozle Deal Audit", "team-1"],
  ["p-wom", "Deal Audit - WOM", "team-1"],
  ["p-updates", "Deal Updates via Chat", "team-1"],
  ["p-campaign", "Campaign optimisations", "team-1"],
  ["p-twc", "TWC optimisation emails", "team-1"],
  ["p-wraps", "Wraps and EOCs", ""],
  ["p-ctv", "CTV research", ""],
  ["p-onboard", "Client onboarding", "team-1"],
].map(([id, name, team], i) => ({
  id,
  name,
  owner_email: ME,
  team_id: team,
  mcp_servers: [],
  pinned: id === "p-twc",
  // Most recently changed first, in the order above.
  created_at: 1_700_000_000 + i,
  updated_at: 1_760_000_000 - i * 3600,
}));

const TOPICS = [
  "Omnicom pacing",
  "Deal desk daily",
  "Incident room",
  "Budget notes",
  "Onboarding plan",
  "Churn analysis",
  "Pricing experiment",
  "Weekly campaign health check for the EMEA region and its partners",
  "Supply path audit",
  "Creative QA",
  "Frequency cap review",
  "Margin report",
];

type Conv = (id: string, title: string, ageSeconds: number, extra?: Record<string, unknown>) => Row;

// A chat last active `ageSeconds` before `NOW`.
function convAt(NOW: number): Conv {
  return (id, title, ageSeconds, extra = {}) => ({
    id,
    title,
    persona: "default",
    model: "test-model",
    pinned: false,
    archived_at: null,
    labels: [],
    updated_at: NOW - ageSeconds,
    created_at: NOW - ageSeconds - 3 * DAY,
    ...extra,
  });
}

// The person's own chats: a few with something to say for themselves, then
// the ton (180 more, spread over eleven weeks).
function ownChats(NOW: number): Row[] {
  const conv = convAt(NOW);
  const own: Row[] = [
    // (has_schedule: the server's listing says which chats a patrol keeps.)
    conv("c-release", "Fleet release watch", 600, { has_schedule: true }),
    conv("c-ctv", "CTV SSP path efficiency deep-dive", DAY + 3600, { has_schedule: true }),
    conv("c-npdeal", "NorthPeak deal optimization", DAY + 7200, { team_visible: true }),
    conv("c-npupd", "North Peak deal updates", 3 * DAY, { has_schedule: true }),
    conv("c-basis", "Basis pacing check", 4 * DAY, { has_schedule: true }),
    conv("c-fw26", "North Peak FW26 campaign", 5 * 3600, { pinned: true, team_visible: true }),
    conv("c-wrap", "End-of-campaign wrap - NorthPeak", 2 * DAY, { pinned: true }),
    conv("c-sealed", "Board pack numbers", 6 * DAY, { lockdown: true }),
    conv("c-link", "Publisher one-pager for Raptive", 4 * DAY + 60, { share_token: "tok123" }),
    conv("c-labels", "Q4 pipeline review", 9 * DAY, { labels: ["finance", "q4"] }),
    conv("c-old", "Index CTV deal test setup", 84 * DAY),
    conv("c-twc1", "TWC weekly optimisation email", 4 * 3600, { project_id: "p-twc" }),
    conv("c-pit1", "Pitch outline: Pella Windows", 12 * DAY, { project_id: "p-pitches" }),
  ];
  for (let i = 0; i < 180; i++) {
    const n = Math.floor(i / TOPICS.length);
    own.push(
      conv(`c-x${i}`, `${TOPICS[i % TOPICS.length]}${n ? ` ${n + 1}` : ""}`, 5 * DAY + i * (DAY / 2.4), {
        project_id: i % 23 === 0 ? "p-campaign" : undefined,
        labels: i % 17 === 0 ? ["client"] : [],
      }),
    );
  }
  return own;
}

function teamChats(NOW: number): Row[] {
  const conv = convAt(NOW);
  const team: Row[] = [
    conv("t-hyvee", "Raptive Hy-Vee deal insights", 1800, { user_email: "jeanne@example.com", team_visible: true }),
    conv("t-twc", "New TWC optimisation emails", 3000, { user_email: "jeanne@example.com", team_visible: true, multiplayer: true }),
    conv("t-f1", "F1 OLV deal names for Index", 4 * DAY, { user_email: "connor@example.com", team_visible: true }),
  ];
  for (let i = 0; i < 31; i++) {
    team.push(
      conv(`t-x${i}`, `${TOPICS[(i * 5) % TOPICS.length]} (team ${i + 1})`, 6 * DAY + i * DAY, {
        user_email: `${["jeanne", "connor", "roman", "maria.fernandez-oliveira"][i % 4]}@example.com`,
        team_visible: true,
        multiplayer: i % 3 === 0,
      }),
    );
  }
  return team;
}

const patrolsAt = (NOW: number) => [
  {
    id: "pt-1",
    conversation_id: "c-npupd",
    title: "Deal delivery check",
    conversation_title: "North Peak deal updates",
    cron: "0 */2 * * *",
    timezone: "America/Detroit",
    status: "active",
    // Half an hour, and a little: it reads "30m" for the test's first minute.
    next_run_at: NOW + 1820,
    run_count: 44,
    approval_mode: "auto",
    mine: true,
    skipped: { reason: "approval_pending", count: 3, slots: [NOW - 7200, NOW - 3600, NOW - 600] },
  },
  {
    id: "pt-2",
    conversation_id: "c-release",
    title: "Fleet release watch (weekdays 8am ET)",
    conversation_title: "Fleet release watch",
    cron: "0 8 * * 1-5",
    timezone: "America/New_York",
    status: "active",
    next_run_at: NOW + 19 * 3600,
    run_count: 12,
    approval_mode: "auto",
    mine: true,
  },
  {
    id: "pt-3",
    conversation_id: "c-ctv",
    title: "CTV path watch",
    conversation_title: "CTV SSP path efficiency deep-dive",
    cron: "0 8 * * 1-5",
    timezone: "America/Detroit",
    status: "active",
    next_run_at: NOW + 20 * 3600,
    run_count: 31,
    approval_mode: "manual",
    mine: true,
  },
  {
    id: "pt-4",
    conversation_id: "c-basis",
    title: "Backfill scan",
    conversation_title: "Basis pacing check",
    cron: "0 9 * * *",
    timezone: "America/Detroit",
    status: "paused",
    run_count: 7,
    approval_mode: "manual",
    mine: true,
  },
  {
    id: "pt-5",
    conversation_id: "t-hyvee",
    title: "Hy-Vee deal insights",
    conversation_title: "Raptive Hy-Vee deal insights",
    cron: "0 */4 * * *",
    timezone: "America/Chicago",
    status: "active",
    next_run_at: NOW + 3 * 3600,
    run_count: 20,
    approval_mode: "autonomous",
    mine: false,
    owner_email: "jeanne@example.com",
    team_visible: true,
  },
];

const PATROL_PROMPT =
  "Goal: keep the team on top of changes to the public repository and say which ones matter to clients and why. Advise only.";
const patrolRunsAt = (NOW: number) => [
  { at: NOW - 3 * 3600, trigger: "schedule", outcome: "queued", run_number: 12 },
  { at: NOW - 27 * 3600, trigger: "schedule", outcome: "busy", reason: "approval_pending" },
  { at: NOW - 51 * 3600, trigger: "manual", outcome: "queued", run_number: 11 },
];

const TASKS = ["Pennington Seed daily refresh", "Raptive client overview refresh", "Pages refresh: Mark Anthony Brands"].map(
  (prompt, i) => ({
    id: `0000000${i}-1111-4111-8111-111111111111`,
    prompt,
    status: "scheduled",
    created_at: new Date(Date.now() - i * 3600e3).toISOString(),
  }),
);

type Layout = Record<string, unknown>;
type Opts = { patrols?: unknown[]; layout?: Layout | null };

async function mockWorkspace(page: Page, opts: Opts = {}) {
  const NOW = nowSeconds();
  const own = ownChats(NOW);
  const team = teamChats(NOW);
  const allPatrols = patrolsAt(NOW);
  const patrols = opts.patrols ?? allPatrols;
  const puts: Layout[] = [];
  const patrolReads: string[] = [];
  await mockChatBoot(page, { conversations: own });
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: ME, role: "member", team_id: "team-1" } }));
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: {
        lockdown_available: true,
        lockdown_only: false,
        lockdown_allowed_models: [],
        conversation_ttl_days: 90,
        conversation_expiry_warning_days: 14,
        conversation_unpinned_cap: 500,
      },
    }),
  );
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: PROJECTS } }));
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols } }));
  await page.route("**/api/patrols/*", (r: Route) => {
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "";
    patrolReads.push(id);
    const patrol = (patrols as Array<{ id: string; mine?: boolean }>).find((p) => p.id === id);
    if (!patrol) return r.fulfill({ status: 404, json: { error: "not found" } });
    // As the server answers: what each run is asked to do, for its creator only.
    return r.fulfill({
      json: { patrol: { ...patrol, ...(patrol.mine ? { prompt: PATROL_PROMPT } : {}) }, runs: patrolRunsAt(NOW) },
    });
  });
  let layout = opts.layout ?? null;
  await page.route("**/api/sidebar", (r: Route) => {
    const method = r.request().method();
    if (method === "PUT") {
      layout = r.request().postDataJSON() as Layout;
      puts.push(layout);
      return r.fulfill({ json: { layout } });
    }
    if (method === "DELETE") {
      layout = null;
      return r.fulfill({ json: { layout: null } });
    }
    return r.fulfill({ json: { layout, workspace_default: null } });
  });
  await page.route("**/api/conversations?*", (r: Route) => {
    const url = new URL(r.request().url());
    if (url.searchParams.get("scope") === "shared") return r.fulfill({ json: { conversations: team } });
    if (url.searchParams.get("scope") === "team") return r.fulfill({ json: { conversations: [] } });
    if (url.searchParams.get("archived") === "true") return r.fulfill({ json: { conversations: [] } });
    return r.fulfill({ json: { conversations: own } });
  });
  // One chat, as the server answers for it: the chat and its schedules (the
  // page's own knowledge of the open chat's patrols, which wins over the
  // patrols list).
  await page.route("**/api/conversations/*", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    const id = new URL(r.request().url()).pathname.split("/").pop() ?? "";
    const found = [...own, ...team].find((c) => c.id === id);
    if (!found) return r.fulfill({ status: 404, json: { error: "not found" } });
    const schedules = (patrols as Array<{ conversation_id: string }>)
      .filter((p) => p.conversation_id === id)
      .map((p) => ({ ...p, kind: "patrol", prompt: PATROL_PROMPT }));
    return r.fulfill({ json: { conversation: found, history: [], schedules } });
  });
  await page.route("**/api/conversations/order", (r: Route) => r.fulfill({ json: { orders: {} } }));
  await page.route("**/api/orchestrator/**", (r: Route) => {
    const path = new URL(r.request().url()).pathname.replace("/api/orchestrator", "");
    if (path === "/me") return r.fulfill({ json: { authenticated: true, username: "e2e" } });
    if (path === "/stats") return r.fulfill({ json: { pending_tasks: 3, running_tasks: 0, completed_tasks_today: 4, failed_tasks_today: 0 } });
    if (path === "/mcp-servers") return r.fulfill({ json: { servers: [] } });
    if (path === "/config") return r.fulfill({ json: { timezone: "UTC" } });
    if (path === "/email-templates") return r.fulfill({ json: [] });
    if (path === "/tasks") return r.fulfill({ json: { data: TASKS, total: TASKS.length, limit: 20, offset: 0 } });
    return r.fulfill({ json: {} });
  });
  return { own, team, puts, patrolReads, allPatrols };
}

async function openChat(page: Page) {
  await page.goto("/chat");
  await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
  await expect(page.getByTestId("pinned-section")).toBeAttached();
}

const sidebar = (page: Page) => page.locator("aside").first();

// Everything inside the sidebar that is a scroll box with something to
// scroll: there must be exactly one, the body.
function scrollBoxes(page: Page) {
  return sidebar(page).evaluate((aside) =>
    [...aside.querySelectorAll<HTMLElement>("*")]
      .filter((n) => {
        const o = getComputedStyle(n).overflowY;
        return (o === "auto" || o === "scroll") && n.scrollHeight > n.clientHeight + 1;
      })
      .map((n) => n.getAttribute("data-testid") ?? n.tagName),
  );
}

const pageOverflowX = (page: Page) =>
  page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);

async function expectNoAxeViolations(page: Page, include: string) {
  const results = await new AxeBuilder({ page })
    .include(include)
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  const summary = results.violations.map(
    (v) => `${v.id} (${v.impact}): ${v.help} — ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
  );
  expect(summary, summary.join("\n")).toEqual([]);
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

// ── A ton of chats: one scroller, nothing sideways, axe — light and dark ────

for (const scheme of ["light", "dark"] as const) {
  test.describe(`${scheme}, on a desktop`, () => {
    test.use({ colorScheme: scheme, viewport: { width: 1440, height: 900 } });

    test("a ton of chats scrolls as one sidebar, with nothing sideways, and passes axe", async ({ page }) => {
      await mockWorkspace(page);
      await openChat(page);

      // Everything is there: the top items, the three lists, a lot of rows.
      await expect(page.getByTestId("nav-to-patrols")).toBeVisible();
      await expect(page.getByTestId("nav-to-orchestrator")).toBeVisible();
      await expect(page.getByTestId("pinned-section").locator("[data-rail-row]")).toHaveCount(3);
      await expect(page.getByTestId("projects-show-all")).toHaveText("All 11 projects");
      const rows = page.getByTestId("unified-chat-list").locator("[data-rail-row]");
      await expect.poll(() => rows.count()).toBeGreaterThan(200);
      // Grouped by date, most recent first (which groups there are depends
      // on the hour the test runs at; their order does not).
      const groups = await page
        .getByTestId("unified-chat-list")
        .locator("[data-date-group]")
        .evaluateAll((els) => els.map((el) => el.getAttribute("data-date-group")));
      expect(groups.length).toBeGreaterThanOrEqual(3);
      expect(groups).toEqual(["today", "yesterday", "week", "older"].filter((g) => groups.includes(g)));
      expect(groups.at(-1)).toBe("older");

      // One sidebar that scrolls as a whole: its body is the only scroll box,
      // and a tall one.
      expect(await scrollBoxes(page)).toEqual(["rail-body"]);
      const body = page.getByTestId("rail-body");
      const { scrollHeight, clientHeight } = await body.evaluate((el) => ({ scrollHeight: el.scrollHeight, clientHeight: el.clientHeight }));
      expect(scrollHeight).toBeGreaterThan(clientHeight * 5);
      // Search, the pen and the + stay put above it; the account stays below.
      await body.evaluate((el) => el.scrollTo(0, el.scrollHeight));
      await expect(rows.last()).toBeInViewport();
      await expect(page.getByTestId("search-input")).toBeInViewport();
      await expect(page.getByTestId("new-chat-button")).toBeInViewport();
      await expect(page.getByTestId("new-menu-button")).toBeInViewport();
      await expect(page.getByTestId("account-menu-button")).toBeInViewport();
      // The top items scroll away with the lists (they are part of the one sidebar).
      await expect(page.getByTestId("nav-to-patrols")).not.toBeInViewport();

      // Nothing sticks out sideways: not the page, not the sidebar.
      expect(await pageOverflowX(page)).toBeLessThanOrEqual(1);
      expect(await body.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);

      await body.evaluate((el) => el.scrollTo(0, 0));
      await expectNoAxeViolations(page, "aside");
    });
  });

  test.describe(`${scheme}, on a phone`, () => {
    // As mobile.spec.ts: a phone viewport with touch, on the suite's Chromium.
    test.use({ colorScheme: scheme, viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

    test("a ton of chats scrolls as one drawer, with nothing sideways, and passes axe", async ({ page }) => {
      await mockWorkspace(page);
      await openChat(page);
      await page.getByRole("button", { name: /open sidebar/i }).click();
      await expect(page.getByTestId("pinned-section")).toBeVisible();

      // The drawer is the same sidebar: search, pen, +, the top items, the lists.
      await expect(page.getByTestId("search-input")).toBeVisible();
      await expect(page.getByTestId("new-chat-button")).toBeVisible();
      await expect(page.getByTestId("new-menu-button")).toBeVisible();
      await expect(page.getByTestId("customize-sidebar-button")).toBeVisible();
      await expect(page.getByTestId("nav-to-patrols")).toBeVisible();
      const rows = page.getByTestId("unified-chat-list").locator("[data-rail-row]");
      await expect.poll(() => rows.count()).toBeGreaterThan(200);

      expect(await scrollBoxes(page)).toEqual(["rail-body"]);
      const body = page.getByTestId("rail-body");
      const { scrollHeight, clientHeight } = await body.evaluate((el) => ({ scrollHeight: el.scrollHeight, clientHeight: el.clientHeight }));
      expect(scrollHeight).toBeGreaterThan(clientHeight * 8);
      // Finger-sized rows.
      const short = await rows.evaluateAll((els) => els.filter((r) => r.getBoundingClientRect().height < 44).length);
      expect(short).toBe(0);
      // The last chat is reachable.
      await rows.last().scrollIntoViewIfNeeded();
      await expect(rows.last()).toBeInViewport();

      expect(await pageOverflowX(page)).toBeLessThanOrEqual(1);
      expect(await body.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);

      await body.evaluate((el) => el.scrollTo(0, 0));
      await expectNoAxeViolations(page, "aside");
    });
  });
}

test.describe("on a phone", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("the drawer cannot be scrolled sideways", async ({ page }) => {
    await mockWorkspace(page);
    await openChat(page);
    await page.getByRole("button", { name: /open sidebar/i }).click();
    await expect(page.getByTestId("pinned-section")).toBeVisible();
    // The drawer clips what sticks out of it, and a clipped box can still be
    // scrolled by the browser (bringing a focused control into view): so
    // nothing inside it may be wider than it is, or the whole drawer shifts
    // sideways and its left edge is cut off.
    const aside = sidebar(page);
    expect(await aside.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
    // …including after a row menu has handed the focus back and the Pinned
    // list has gone into reorder mode.
    await page.getByTestId("pinned-reorder-toggle").tap();
    await expect(page.getByTestId("chat-reorder-list")).toBeVisible();
    expect(await aside.evaluate((el) => el.scrollLeft)).toBe(0);
    const handle = page.getByTestId("chat-reorder-list").locator("li").first().locator('[data-testid^="chat-reorder-handle-"]');
    expect((await handle.boundingBox())!.x).toBeGreaterThanOrEqual(0);
  });

  test("in dashboard view the header's button opens the drawer", async ({ page }) => {
    await mockWorkspace(page);
    await page.goto("/chat?c=c-release&view=dashboard");
    await expect(page.getByTestId("dashboard-hud")).toBeVisible({ timeout: 15_000 });
    // No strip on a phone: the sidebar is the drawer, opened from the header.
    await expect(page.getByTestId("search-input")).not.toBeInViewport();
    await page.getByTestId("dashboard-sidebar-button").tap();
    await expect(page.getByTestId("search-input")).toBeInViewport();
    await expect(page.getByTestId("nav-to-patrols")).toBeInViewport();
    await page.keyboard.press("Escape");
    await expect(page.getByTestId("search-input")).not.toBeInViewport();
    await expect(page.getByTestId("dashboard-hud")).toBeVisible();
  });
});

// ── The menus ───────────────────────────────────────────────────────────────

test.describe("on a desktop", () => {
  test.use({ viewport: { width: 1440, height: 900 } });

  test("the + menu, the Chats menu and a row's ⋯ menu list what they should", async ({ page }) => {
    await mockWorkspace(page);
    await openChat(page);

    // + : everything a person can start.
    await page.getByTestId("new-menu-button").click();
    const plus = page.getByRole("menu", { name: "New" });
    await expect(plus.getByRole("menuitem")).toHaveText([
      /^New chat/,
      /^New patrol/,
      /^New background task/,
      "New project",
      "New sealed chat",
    ]);
    await expect(plus.getByRole("separator")).toHaveCount(1);
    await expect(page.getByTestId("new-menu-chat")).toContainText("Talk it through with an agent.");
    await expect(page.getByTestId("new-menu-patrol")).toContainText("A chat that checks in on a schedule.");
    await expect(page.getByTestId("new-menu-task")).toContainText("Runs on its own, without a chat.");
    await page.keyboard.press("Escape");
    await expect(plus).toHaveCount(0);
    await expect(page.getByTestId("new-menu-button")).toBeFocused();

    // Chats: sort, filter by label, select, and the one bulk action.
    const chatsMenuButton = page.getByTestId("recent-sort");
    await chatsMenuButton.scrollIntoViewIfNeeded();
    await chatsMenuButton.click();
    const chatsMenu = page.getByRole("menu", { name: "Chat list options" });
    await expect(chatsMenu.getByRole("menuitemradio")).toHaveText(["Recent activity", "Newest first", "Title A–Z"]);
    await expect(page.getByTestId("recent-sort-activity")).toHaveAttribute("aria-checked", "true");
    await expect(chatsMenu.getByRole("menuitem")).toHaveText(["Filter by label", "Select chats…", /^Delete temporary chats…/]);
    await expect(page.getByTestId("delete-temporary-chats")).toContainText(
      "Keeps pinned, filed, shared and patrol chats. Temporary chats go after 90 days without activity anyway.",
    );
    // The labels the person uses, with how many chats carry each.
    await page.getByTestId("chats-menu-labels").click();
    await expect(page.getByRole("menu", { name: "Filter by label" }).getByRole("menuitemcheckbox")).toHaveText([
      /client\s*11/,
      /finance\s*1/,
      /q4\s*1/,
    ]);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(chatsMenu).toHaveCount(0);

    // A pinned chat's ⋯ : grouped, plain verbs.
    const row = page.locator('[data-conversation-id="c-fw26"]').first();
    await row.scrollIntoViewIfNeeded();
    await row.hover();
    await row.getByRole("button", { name: "Conversation options for North Peak FW26 campaign" }).click();
    const rowMenu = page.getByRole("menu", { name: "Options for North Peak FW26 campaign" });
    await expect(rowMenu.getByRole("menuitem")).toHaveText([
      "Unpin",
      "Move to the top",
      "Rename",
      "Move to project",
      "Labels",
      "Set as start page",
      "Add a patrol…",
      "Make a background task…",
      "Save as workflow…",
      "Share…",
      "Download chat…",
      "Transfer ownership…",
      "Archive",
      "Delete…",
    ]);
    await expect(rowMenu.getByRole("separator")).toHaveCount(4);
    // The menu is on screen, whole.
    const box = (await rowMenu.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.y + box.height).toBeLessThanOrEqual(900 + 1);
    await page.keyboard.press("Escape");

    // A patrol chat's menu leads to its patrols instead of offering a first one.
    const patrolRow = page.locator('[data-conversation-id="c-npupd"]').first();
    await patrolRow.scrollIntoViewIfNeeded();
    await patrolRow.hover();
    await patrolRow.getByRole("button", { name: /^Conversation options for / }).click();
    await expect(page.getByTestId("chat-menu-patrol")).toHaveText("Patrols…");
  });

  test("rows are a title and at most one status", async ({ page }) => {
    await mockWorkspace(page);
    await openChat(page);
    const status = (id: string) => page.locator(`[data-conversation-id="${id}"]`).first().getByTestId("row-status");

    // A patrol chat: when it checks next (and that wins over everything else it is).
    await expect(status("c-release")).toHaveAttribute("data-status", "patrol");
    await expect(status("c-release")).toHaveText("19h");
    await expect(status("c-basis")).toHaveAttribute("data-status", "patrol-paused");
    await expect(status("c-basis")).toHaveText("Paused");
    // Shared with the team, shared by link, sealed.
    await expect(status("c-npdeal")).toHaveAttribute("data-status", "team");
    await expect(status("c-link")).toHaveAttribute("data-status", "link");
    await expect(status("c-sealed")).toHaveAttribute("data-status", "sealed");
    // A temporary chat inside the warning window: the days left.
    await expect(status("c-old")).toHaveAttribute("data-status", "expiring");
    await expect(status("c-old")).toHaveText("6d");
    // A teammate's chat: whose it is; a Multiplayer one is live as well.
    await expect(status("t-hyvee")).toHaveAttribute("data-status", "patrol");
    await expect(status("t-hyvee").getByTestId("row-owner")).toHaveText("J");
    await expect(status("t-twc")).toHaveAttribute("data-status", "live");
    // An ordinary chat says nothing; labels stay off the row (in its tooltip).
    await expect(status("c-x1")).toHaveCount(0);
    await expect(status("c-labels")).toHaveCount(0);
    await expect(page.locator('[data-conversation-id="c-labels"] [data-rail-row]').first()).toHaveAttribute("title", /finance, q4/);
    // What a status means reaches a screen reader as the row's description,
    // never as part of its name.
    const patrolRow = sidebar(page).getByRole("button", { name: "Fleet release watch", exact: true });
    await expect(patrolRow).toHaveAccessibleDescription(/Patrol — next check in 19h/);

    // The chips narrow the list to whose chats (or which kind) are shown.
    const rows = page.getByTestId("unified-chat-list").locator("[data-rail-row]");
    await expect(page.getByTestId("chat-scope").getByRole("button")).toHaveText(["All", "Mine", "Shared", "Patrols"]);
    await page.getByTestId("chat-scope-shared").click();
    await expect(rows).toHaveCount(34);
    await page.getByTestId("chat-scope-patrols").click();
    await expect(rows).toHaveCount(5);
    await page.getByTestId("chat-scope-all").click();
  });

  // ── The gear ──────────────────────────────────────────────────────────────

  test("the gear's panel changes density and what shows, and leads to the full panel", async ({ page }) => {
    const { puts } = await mockWorkspace(page);
    await openChat(page);
    const aside = sidebar(page);
    await expect(aside).toHaveAttribute("data-density", "comfortable");
    const rowHeight = () =>
      page.locator('[data-conversation-id="c-ctv"] [data-rail-row]').first().evaluate((el) => el.getBoundingClientRect().height);
    const comfortable = await rowHeight();

    const gear = page.getByTestId("customize-sidebar-button");
    await gear.click();
    const panel = page.getByTestId("customize-popover");
    await expect(panel).toBeVisible();
    await expect(panel.getByRole("heading", { name: "Customize sidebar" })).toBeVisible();
    // Where fleet opens: the last chat by default; what is at the top and
    // what is pinned are offered.
    const home = panel.getByTestId("customize-home");
    await expect(home.getByRole("radio", { checked: true })).toHaveCount(1);
    await expect(panel.getByTestId("customize-home-option").first()).toContainText("The last chat you had open");
    await expect(panel.getByTestId("customize-home-option").first()).toHaveAttribute("data-selected", "true");
    await expect(panel.getByTestId("customize-home-option").filter({ hasText: "Background tasks" })).toHaveCount(1);
    await expect(panel.getByTestId("customize-home-option").filter({ hasText: "North Peak FW26 campaign" })).toContainText("Chat");
    await expect(panel.getByTestId("customize-save-state")).toHaveText("Saved to your account, on every device.");
    await expectNoAxeViolations(page, '[data-testid="customize-popover"]');

    // Density: the rows get shorter at once, and it is saved.
    await panel.getByRole("button", { name: "Compact" }).click();
    await expect(aside).toHaveAttribute("data-density", "compact");
    await expect.poll(rowHeight).toBeLessThan(comfortable);
    await expect.poll(() => (puts.at(-1) as { compact?: boolean } | undefined)?.compact).toBe(true);
    await panel.getByRole("button", { name: "Comfortable" }).click();
    await expect(aside).toHaveAttribute("data-density", "comfortable");
    await expect.poll(rowHeight).toBe(comfortable);

    // Show: Patrols leaves the top of the sidebar and comes back.
    const patrolsSwitch = panel.getByTestId("customize-show-patrols");
    await expect(patrolsSwitch).toBeChecked();
    await patrolsSwitch.click();
    await expect(page.getByTestId("nav-to-patrols")).toHaveCount(0);
    await expect
      .poll(() => (puts.at(-1) as { items?: { id: string; hidden?: boolean }[] } | undefined)?.items?.find((i) => i.id === "patrols")?.hidden)
      .toBe(true);
    await patrolsSwitch.click();
    await expect(page.getByTestId("nav-to-patrols")).toBeVisible();
    // …and so do Projects and teammates' chats.
    await panel.getByTestId("customize-show-projects").click();
    await expect(page.getByTestId("projects-section")).toHaveCount(0);
    await panel.getByTestId("customize-show-projects").click();
    await expect(page.getByTestId("projects-section")).toBeVisible();
    await panel.getByTestId("customize-show-team").click();
    await expect(page.getByTestId("team-shared-conv-row")).toHaveCount(0);
    await panel.getByTestId("customize-show-team").click();
    await expect(page.getByTestId("team-shared-conv-row").first()).toBeAttached();

    // A start page: fleet opens to Background tasks.
    await panel.getByTestId("customize-home-option").filter({ hasText: "Background tasks" }).click();
    await expect.poll(() => (puts.at(-1) as { home?: string } | undefined)?.home).toBe("tasks");
    await panel.getByTestId("customize-home-option").first().click();
    await expect.poll(() => (puts.at(-1) as { home?: string } | undefined)?.home ?? "").toBe("");

    // More options: the popover closes and the full panel opens.
    await panel.getByTestId("customize-more").click();
    await expect(panel).toHaveCount(0);
    const full = page.getByTestId("sidebar-customizer");
    await expect(full).toBeVisible();
    await expect(full.getByTestId("sidebar-density")).toBeVisible();
    await expect(full.getByTestId("sidebar-editor-pinned").locator("li")).toHaveCount(3);
    await full.getByTestId("sidebar-done").click();
    await expect(full).toHaveCount(0);

    // Escape closes the popover and hands the focus back to the gear.
    await gear.click();
    await expect(panel).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panel).toHaveCount(0);
    await expect(gear).toBeFocused();
  });

  test("for an admin the full panel leads to the workspace default, and passes axe", async ({ page }) => {
    await mockWorkspace(page);
    // The admin probe (the most recent route wins).
    await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ json: { settings: [] } }));
    await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: ME, role: "admin", team_id: "team-1" } }));
    await page.route("**/api/admin/sidebar", (r: Route) => r.fulfill({ json: { layout: null, updated_by: "", updated_at: 0 } }));
    await openChat(page);
    await page.getByTestId("customize-sidebar-button").click();
    await page.getByTestId("customize-more").click();
    const full = page.getByTestId("sidebar-customizer");
    await expect(full).toBeVisible();
    // Two sidebars an admin can edit: their own (this panel) and the one
    // everyone starts from.
    const scope = full.getByTestId("sidebar-scope");
    await expect(scope.locator('[aria-current="page"]')).toHaveText("My sidebar");
    await expect(scope.getByRole("link", { name: "Workspace default" })).toHaveAttribute("href", "/settings/admin/sidebar");
    // The panel's parts, top to bottom.
    await expect(full.getByRole("heading", { level: 3 })).toHaveText([
      "Fleet opens to",
      "Start from",
      "Top of the sidebar",
      "Pinned",
      "Lists",
      "Density",
    ]);
    await expect(full.getByTestId("sidebar-editor-sections").locator("li")).toHaveCount(3);
    await expect(full.getByTestId("sidebar-editor-dates")).toBeChecked();
    await expect(full.getByTestId("sidebar-editor-status")).toBeChecked();
    await expectNoAxeViolations(page, '[data-testid="sidebar-customizer"]');

    // The two row options change the list at once.
    await full.getByTestId("sidebar-editor-status").click();
    await expect(page.getByTestId("unified-chat-list").getByTestId("row-status")).toHaveCount(0);
    await full.getByTestId("sidebar-editor-status").click();
    await expect(page.getByTestId("unified-chat-list").getByTestId("row-status").first()).toBeAttached();
    await full.getByTestId("sidebar-editor-dates").click();
    await expect(page.getByTestId("unified-chat-list").locator("[data-date-group]")).toHaveCount(0);
    await full.getByTestId("sidebar-editor-dates").click();
    await expect(page.getByTestId("unified-chat-list").locator("[data-date-group]").first()).toBeAttached();

    // Workspace default is its own page, in Settings.
    await scope.getByRole("link", { name: "Workspace default" }).click();
    await expect(page).toHaveURL(/\/settings\/admin\/sidebar$/);
    await expect(page.getByRole("heading", { name: "Sidebar", exact: true })).toBeVisible();
    await expect(page.getByTestId("sidebar-customizer")).toHaveCount(0);
  });

  // ── Resize ────────────────────────────────────────────────────────────────

  test("the sidebar's edge resizes it: by dragging, from the keyboard, remembered, and reset", async ({ page }) => {
    await mockWorkspace(page);
    await openChat(page);
    const aside = sidebar(page);
    const width = async () => Math.round((await aside.boundingBox())!.width);
    const handle = page.getByTestId("rail-resize");
    await expect(handle).toHaveRole("separator");
    await expect(handle).toHaveAccessibleName("Resize sidebar");
    await expect(handle).toHaveAttribute("aria-valuemin", "240");
    await expect(handle).toHaveAttribute("aria-valuemax", "480");
    expect(await width()).toBe(300);
    await expect(handle).toHaveAttribute("aria-valuenow", "300");

    // A pointer drags the edge; the sidebar follows it.
    const hb = (await handle.boundingBox())!;
    const x = hb.x + hb.width / 2;
    await page.mouse.move(x, 420);
    await page.mouse.down();
    await page.mouse.move(x + 60, 420, { steps: 4 });
    await expect(handle).toHaveAttribute("data-dragging", "true");
    await page.mouse.move(x + 100, 430, { steps: 4 });
    await page.mouse.up();
    await expect(handle).not.toHaveAttribute("data-dragging", "true");
    await expect.poll(width).toBe(400);
    await expect(handle).toHaveAttribute("aria-valuenow", "400");
    // The page beside it gives way; nothing overflows.
    expect(await pageOverflowX(page)).toBeLessThanOrEqual(1);
    // It never goes past its limits, however far the pointer goes.
    const hb2 = (await handle.boundingBox())!;
    await page.mouse.move(hb2.x + hb2.width / 2, 420);
    await page.mouse.down();
    await page.mouse.move(1300, 420, { steps: 6 });
    await page.mouse.up();
    await expect.poll(width).toBe(480);

    // From the keyboard: arrows by a step, Home and End to the limits.
    await handle.focus();
    await page.keyboard.press("ArrowLeft");
    await expect.poll(width).toBe(464);
    await page.keyboard.press("ArrowLeft");
    await expect.poll(width).toBe(448);
    await page.keyboard.press("ArrowRight");
    await expect.poll(width).toBe(464);
    await page.keyboard.press("Home");
    await expect.poll(width).toBe(240);
    await page.keyboard.press("ArrowLeft");
    await expect.poll(width).toBe(240);
    await page.keyboard.press("End");
    await expect.poll(width).toBe(480);
    await page.keyboard.press("ArrowRight");
    await expect.poll(width).toBe(480);
    // (Each step is let settle before the next: the sidebar eases to its new
    // width, and a step is taken from where it is drawn.)
    for (const expected of [464, 448, 432]) {
      await page.keyboard.press("ArrowLeft");
      await expect.poll(width).toBe(expected);
      await expect(handle).toHaveAttribute("aria-valuenow", String(expected));
    }

    // Remembered on this device.
    await page.reload();
    await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
    await expect.poll(width).toBe(432);
    await expect(page.getByTestId("rail-resize")).toHaveAttribute("aria-valuenow", "432");

    // A double-click puts it back.
    await page.getByTestId("rail-resize").dblclick();
    await expect.poll(width).toBe(300);
    await page.reload();
    await page.getByRole("heading", { name: /what can i help with/i }).waitFor({ timeout: 15_000 });
    await expect.poll(width).toBe(300);
  });

  // ── The collapsed strip ───────────────────────────────────────────────────

  test("collapsed, the strip holds the expand button, the +, the top items and the account", async ({ page }) => {
    await mockWorkspace(page, {
      layout: {
        version: 1,
        items: [
          { id: "chat", kind: "builtin" },
          { id: "patrols", kind: "builtin" },
          { id: "tasks", kind: "builtin" },
          { id: "s-board", kind: "chat", ref: "c-ctv", title: "CTV SSP path efficiency deep-dive", view: "dashboard" },
        ],
        sections: [{ id: "pinned" }, { id: "projects" }, { id: "chats" }, { id: "team" }],
      },
    });
    await openChat(page);
    const aside = sidebar(page);
    await page.getByTestId("rail-collapse").click();
    await expect.poll(async () => (await aside.boundingBox())!.width).toBeLessThan(80);

    // What is left: the way to expand it, the +, the top items, the account.
    await expect(page.getByTestId("rail-expand")).toBeVisible();
    await expect(page.getByTestId("rail-expand")).toHaveAccessibleName("Expand sidebar");
    await expect(page.getByTestId("new-menu-button")).toBeVisible();
    await expect(page.getByTestId("nav-to-patrols")).toBeVisible();
    await expect(page.getByTestId("nav-to-orchestrator")).toBeVisible();
    await expect(page.getByTestId("rail-shortcut")).toBeVisible();
    await expect(page.getByTestId("account-menu-button")).toBeVisible();
    // What is not: the gear, search, the pen, the lists, the edge to drag.
    await expect(page.getByTestId("customize-sidebar-button")).toBeHidden();
    await expect(page.getByTestId("search-input")).toBeHidden();
    await expect(page.getByTestId("new-chat-button")).toBeHidden();
    await expect(page.getByTestId("pinned-section")).toBeHidden();
    await expect(page.getByTestId("chats-section")).toBeHidden();
    await expect(page.getByTestId("rail-resize")).toHaveCount(0);
    expect(await scrollBoxes(page)).toEqual([]);
    expect(await pageOverflowX(page)).toBeLessThanOrEqual(1);

    // Each icon says what it is beside the strip, on hover and on focus.
    await page.getByTestId("nav-to-patrols").hover();
    await expect(page.locator(".rail-tip")).toContainText("Patrols");
    await page.mouse.move(700, 400);
    await expect(page.locator(".rail-tip")).toHaveCount(0);

    // The + still starts a chat from here; its menu opens beside the strip.
    await page.getByTestId("new-menu-button").click();
    const menu = page.getByRole("menu", { name: "New" });
    await expect(menu).toBeVisible();
    const mb = (await menu.boundingBox())!;
    const pb = (await page.getByTestId("new-menu-button").boundingBox())!;
    expect(mb.x).toBeGreaterThanOrEqual(pb.x + pb.width);
    expect(Math.abs(mb.y - pb.y)).toBeLessThanOrEqual(8);
    await expect(page.getByTestId("new-menu-chat")).toBeVisible();
    await page.keyboard.press("Escape");

    await expectNoAxeViolations(page, "aside");

    // Expanding brings everything back.
    await page.getByTestId("rail-expand").click();
    await expect(page.getByTestId("search-input")).toBeVisible();
    await expect(page.getByTestId("rail-collapse")).toHaveAccessibleName("Collapse sidebar");
    await expect(page.getByTestId("customize-sidebar-button")).toBeVisible();
  });

  // ── The Patrols page ──────────────────────────────────────────────────────

  test("the Patrols page lists my patrols and the ones shared with me", async ({ page }) => {
    const { patrolReads } = await mockWorkspace(page);
    await openChat(page);
    await page.getByTestId("nav-to-patrols").click();
    await expect(page).toHaveURL(/\/patrols$/);
    await expect(page.getByRole("heading", { name: "Patrols", exact: true })).toBeVisible();
    // The same sidebar, with Patrols marked as where the person is.
    await expect(page.getByTestId("nav-to-patrols")).toHaveAttribute("aria-current", "page");
    await expect(page.getByTestId("pinned-section")).toBeVisible();

    const mine = page.getByRole("list", { name: "Your patrols" }).getByTestId("patrol-card");
    await expect(mine).toHaveCount(4);
    await expect(page.getByTestId("patrols-count")).toHaveText("5 patrols");
    const shared = page.getByRole("region", { name: "Shared with you" }).getByTestId("patrol-card");
    await expect(shared).toHaveCount(1);

    // A row like the one in a chat's patrol dialog: its name, schedule, run
    // count and approval mode, the chat it lives in, and when it checks next.
    const release = page.locator('[data-patrol-id="pt-2"]');
    await expect(release).toContainText("Fleet release watch (weekdays 8am ET)");
    await expect(release).toContainText("Run #12");
    await expect(release).toContainText("Auto-approve");
    await expect(release).toContainText("Fleet release watch");
    await expect(release.getByTestId("patrol-next")).toHaveText("in 19h");
    await expect(release.getByTestId("patrol-run-now")).toBeVisible();
    await expect(release.getByTestId("patrol-pause")).toHaveAccessibleName("Pause Fleet release watch (weekdays 8am ET)");
    // A paused one says so, and offers to resume; one being skipped says why, once.
    const paused = page.locator('[data-patrol-id="pt-4"]');
    await expect(paused.getByTestId("patrol-next")).toHaveText("paused");
    await expect(paused.getByTestId("patrol-pause")).toHaveAccessibleName("Resume Backfill scan");
    const skipped = page.locator('[data-patrol-id="pt-1"]');
    await expect(skipped.getByTestId("patrol-skipped")).toHaveText("3 runs skipped · approval waiting");
    await expect(skipped.getByTestId("patrol-next")).toHaveText("in 30m");

    // A teammate's patrol: whose it is, and only a way to its chat.
    const theirs = shared.first();
    await expect(theirs).toContainText("Hy-Vee deal insights");
    await expect(theirs.getByTestId("row-owner")).toHaveText("J");
    await expect(theirs.getByText("Shared by jeanne@example.com")).toBeAttached();
    await expect(theirs.getByRole("button", { name: "Open chat" }).or(theirs.getByRole("link", { name: "Open chat" }))).toHaveCount(1);
    await expect(theirs.getByTestId("patrol-run-now")).toHaveCount(0);
    await expect(theirs.getByTestId("patrol-pause")).toHaveCount(0);

    // Opening a row reads that patrol: what each run does, and its recent runs.
    expect(patrolReads).toEqual([]);
    await release.getByRole("button", { expanded: false }).first().click();
    await expect.poll(() => patrolReads).toEqual(["pt-2"]);
    await expect(release.getByTestId("patrol-prompt")).toContainText("Goal: keep the team on top of changes");
    await expect(release.getByTestId("patrol-runs").locator("li")).toHaveText([
      /Ran on schedule · Run #12/,
      /Skipped: an approval was waiting/,
      /Ran when asked \(Run now\) · Run #11/,
    ]);
    await expect(release.getByTestId("patrol-edit")).toBeVisible();
    await expect(release.getByTestId("patrol-remove")).toHaveText("Remove patrol");

    // A teammate's opens too, read-only: no task text (it is its owner's), no
    // edit, no remove.
    await theirs.getByRole("button", { expanded: false }).first().click();
    await expect.poll(() => patrolReads).toEqual(["pt-2", "pt-5"]);
    await expect(theirs.getByTestId("patrol-runs").locator("li")).toHaveCount(3);
    await expect(theirs).toContainText("jeanne@example.com set this patrol up. You can read its chat; only they can run, pause or change it.");
    await expect(theirs.getByTestId("patrol-prompt")).toHaveCount(0);
    await expect(theirs.getByTestId("patrol-edit")).toHaveCount(0);
    await expect(theirs.getByTestId("patrol-remove")).toHaveCount(0);

    await expectNoAxeViolations(page, '[data-testid="patrols-page"]');

    // Search narrows the list across both groups.
    await page.getByTestId("patrols-search").fill("hy-vee");
    await expect(page.getByTestId("patrol-card")).toHaveCount(1);
    await expect(page.getByTestId("patrols-count")).toHaveText("1 of 5");

    // New patrol opens the short form; the guided setup is one press on.
    await page.getByTestId("patrols-search").fill("");
    await page.getByTestId("patrols-new").click();
    await expect(page.getByTestId("patrols-guided")).toBeVisible();
    await expect(page.getByRole("group", { name: "Runs in" }).getByRole("button")).toHaveText(["A new chat", "An existing chat"]);
  });

  test("with no patrols, the Patrols page says what one is and offers the guided setup", async ({ page }) => {
    await mockWorkspace(page, { patrols: [] });
    await page.goto("/patrols");
    const empty = page.getByTestId("patrols-empty");
    await expect(empty).toBeVisible({ timeout: 15_000 });
    await expect(empty.getByRole("heading", { name: "Set up your first patrol" })).toBeVisible();
    await expect(empty).toContainText("A patrol is a chat that checks your tools on a schedule");
    await expect(page.getByTestId("patrol-card")).toHaveCount(0);
    await expect(page.getByTestId("patrols-search")).toHaveCount(0);
    await expectNoAxeViolations(page, '[data-testid="patrols-page"]');
    // "Or write one yourself" opens the short form in the page…
    await page.getByTestId("patrols-write").click();
    await expect(page.getByTestId("patrols-guided")).toBeVisible();
    // …and the main button opens the wizard.
    await page.getByRole("button", { name: "Cancel" }).click();
    await page.getByTestId("patrols-first").click();
    await expect(page.getByRole("dialog", { name: "Patrol Launchpad" })).toBeVisible();
  });

  test("a failed read of the patrols says so, and never reads as having none", async ({ page }) => {
    const { allPatrols } = await mockWorkspace(page);
    let fail = true;
    await page.route("**/api/patrols", (r: Route) =>
      fail ? r.fulfill({ status: 502, body: "down" }) : r.fulfill({ json: { patrols: allPatrols } }),
    );
    await page.goto("/patrols");
    await expect(page.getByTestId("patrols-load-failed")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("patrols-empty")).toHaveCount(0);
    fail = false;
    await page.getByTestId("patrols-load-failed").getByRole("button", { name: "Try again" }).click();
    await expect(page.getByTestId("patrol-card")).toHaveCount(5);
  });

  // ── The same sidebar on Background tasks ──────────────────────────────────

  test("Background tasks keeps the sidebar, and a chat row there leads back to the chat", async ({ page }) => {
    const { own } = await mockWorkspace(page);
    await page.goto("/orchestrator");
    await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible({ timeout: 15_000 });

    // The page itself is as it was: its header button and its list of tasks.
    await expect(page.getByTestId("new-task-btn")).toBeVisible();
    for (const t of TASKS) await expect(page.getByTestId("orchestrator-dashboard").getByText(t.prompt).first()).toBeVisible();

    // The same sidebar, with Background tasks marked as where the person is.
    await expect(page.getByTestId("nav-to-orchestrator")).toHaveAttribute("aria-current", "page");
    await expect(page.getByTestId("search-input")).toBeVisible();
    await expect(page.getByTestId("new-chat-button")).toBeVisible();
    await expect(page.getByTestId("new-menu-button")).toBeVisible();
    await expect(page.getByTestId("customize-sidebar-button")).toBeVisible();
    await expect(page.getByTestId("pinned-section").locator("[data-rail-row]")).toHaveCount(3);
    await expect(page.getByTestId("projects-section")).toBeVisible();
    const rows = page.getByTestId("unified-chat-list").locator("[data-rail-row]");
    await expect.poll(() => rows.count()).toBeGreaterThan(200);
    expect(await scrollBoxes(page)).toEqual(["rail-body"]);
    expect(await pageOverflowX(page)).toBeLessThanOrEqual(1);
    await expectNoAxeViolations(page, "aside");

    // Its + offers a background task from here too (it opens the page's own form).
    await page.getByTestId("new-menu-button").click();
    await expect(page.getByTestId("new-menu-task")).toBeVisible();
    await page.keyboard.press("Escape");

    // A chat row leads back to that chat, without a full page load.
    await page.evaluate(() => ((window as unknown as { __stayed?: boolean }).__stayed = true));
    const target = own.find((c) => c.id === "c-ctv")!;
    await page.locator('[data-conversation-id="c-ctv"] [data-rail-row]').first().click();
    await expect(page).toHaveURL(/\/chat\?c=c-ctv$/);
    await expect(page.locator("main").getByText(target.title)).toBeVisible({ timeout: 15_000 });
    expect(await page.evaluate(() => (window as unknown as { __stayed?: boolean }).__stayed)).toBe(true);
    await expect(page.locator('[data-conversation-id="c-ctv"]').first()).toHaveClass(/rail-active/);

    // And the pen starts a new chat from any page.
    await page.getByTestId("nav-to-orchestrator").click();
    await expect(page.getByTestId("orchestrator-dashboard")).toBeVisible();
    await page.getByTestId("new-chat-button").click();
    await expect(page).toHaveURL(/\/chat/);
    await expect(page.getByRole("heading", { name: /what can i help with/i })).toBeVisible();
  });

  test("the + on another page starts a background task on its own page", async ({ page }) => {
    await mockWorkspace(page);
    await page.goto("/help");
    await expect(page.getByTestId("guide-card-chat")).toBeVisible({ timeout: 15_000 });
    await page.getByTestId("new-menu-button").click();
    await page.getByTestId("new-menu-task").click();
    await expect(page).toHaveURL(/\/orchestrator/);
    await expect(page.getByRole("dialog").filter({ has: page.getByTestId("task-mcp-section") })).toBeVisible({ timeout: 15_000 });
  });

  // ── Dashboard view ────────────────────────────────────────────────────────

  test("dashboard view keeps the strip beside the board; expanding slides the sidebar over it", async ({ page }) => {
    await mockWorkspace(page);
    await page.goto("/chat?c=c-release&view=dashboard");
    await expect(page.getByTestId("dashboard-hud")).toBeVisible({ timeout: 15_000 });
    const aside = sidebar(page);
    const hudLeft = async () => Math.round((await page.getByTestId("dashboard-hud").boundingBox())!.x);

    // The strip: always a way back to a chat.
    await expect.poll(async () => (await aside.boundingBox())!.width).toBeLessThan(80);
    await expect(page.getByTestId("rail-expand")).toBeVisible();
    await expect(page.getByTestId("new-menu-button")).toBeVisible();
    await expect(page.getByTestId("nav-to-patrols")).toBeVisible();
    await expect(page.getByTestId("search-input")).toBeHidden();
    // The header's own sidebar button is the phone's.
    await expect(page.getByTestId("dashboard-sidebar-button")).toBeHidden();
    const boardAt = await hudLeft();
    expect(boardAt).toBeGreaterThan(40);

    // Expanding slides the whole sidebar in over the board, above a scrim;
    // the board does not move under it.
    await page.getByTestId("rail-expand").click();
    await expect(page.getByTestId("search-input")).toBeVisible();
    await expect(page.getByTestId("dashboard-rail-scrim")).toBeVisible();
    await expect.poll(async () => (await aside.boundingBox())!.width).toBeGreaterThan(280);
    expect(await hudLeft()).toBe(boardAt);
    await expect(page.getByTestId("pinned-section")).toBeVisible();
    await expect(page.getByTestId("rail-resize")).toHaveCount(0);

    // Escape closes the slide-over, not the view, and the board stays put
    // through every frame of the close: the sidebar going back to the strip
    // must never push it right and slide it back.
    const framesWhileClosing = page.evaluate(
      () =>
        new Promise<number[]>((resolve) => {
          const hud = document.querySelector('[data-testid="dashboard-hud"]')!;
          const xs: number[] = [];
          const start = performance.now();
          const tick = () => {
            xs.push(Math.round(hud.getBoundingClientRect().x));
            if (performance.now() - start < 500) requestAnimationFrame(tick);
            else resolve(xs);
          };
          requestAnimationFrame(tick);
        }),
    );
    await page.keyboard.press("Escape");
    expect(new Set(await framesWhileClosing)).toEqual(new Set([boardAt]));
    await expect(page.getByTestId("search-input")).toBeHidden();
    await expect(page.getByTestId("dashboard-rail-scrim")).toHaveCount(0);
    await expect(page.getByTestId("dashboard-hud")).toBeVisible();
    await expect.poll(async () => (await aside.boundingBox())!.width).toBeLessThan(80);
    expect(await hudLeft()).toBe(boardAt);
    await expect(page.getByTestId("rail-expand")).toBeFocused();

    // So does a click on the scrim.
    await page.getByTestId("rail-expand").click();
    await expect(page.getByTestId("dashboard-rail-scrim")).toBeVisible();
    await page.getByTestId("dashboard-rail-scrim").click({ position: { x: 1000, y: 500 } });
    await expect(page.getByTestId("dashboard-rail-scrim")).toHaveCount(0);
    await expect(page.getByTestId("dashboard-hud")).toBeVisible();

    // Opening it there does not change the choice made for the chat view:
    // leaving the dashboard, the sidebar is expanded as it was.
    await page.getByTestId("dashboard-exit").click();
    await expect(page.getByTestId("dashboard-hud")).toHaveCount(0);
    await expect(page.getByTestId("search-input")).toBeVisible();
    await expect(page.getByTestId("rail-collapse")).toBeVisible();
  });
});
