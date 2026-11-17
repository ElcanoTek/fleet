import { test, expect, type Page, type Route } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// The customizable sidebar (docs/SIDEBAR.md): the default layout is the rail
// as it always was; a saved layout pins shortcuts (chats, dashboards,
// projects, pages), hides, renames and reorders the destinations and the chat
// page's sections, and names a Home that opens when fleet is opened; the
// Customize panel edits it live and saves it with PUT /api/sidebar; a
// workspace default applies to anyone without their own.
//
// SIDEBAR_SHOTS=<dir> also saves a screenshot of each configuration there
// (how the PR's screenshots were made); the assertions are the test.

const SHOTS = process.env.SIDEBAR_SHOTS ?? "";

const CONVERSATIONS = [
  { id: "c1", title: "Q3 pipeline review", pinned: true },
  { id: "c2", title: "Revenue board", pinned: true },
  { id: "c3", title: "Ops standup", pinned: false, project_id: "p1" },
  { id: "c4", title: "Draft launch email", pinned: false },
  { id: "c5", title: "Vendor comparison", pinned: false },
  {
    id: "c9",
    title: "All-hands war room",
    pinned: false,
    multiplayer: true,
    user_email: "ops@example.com",
  },
];

type Layout = Record<string, unknown>;

async function setup(
  page: Page,
  opts: { own?: Layout | null; workspace?: Layout | null; admin?: boolean; role?: string } = {},
) {
  const puts: Layout[] = [];
  await mockChatBoot(page, { conversations: CONVERSATIONS });
  if (opts.admin) {
    await page.route("**/api/admin/settings", (r: Route) => r.fulfill({ json: { settings: [] } }));
  }
  await page.route("**/api/me", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com", role: opts.role ?? "member" } }),
  );
  await page.route("**/api/projects", (r: Route) =>
    r.fulfill({
      json: {
        projects: [
          { id: "p1", owner_email: "e2e@example.com", name: "Launch plan", mcp_servers: [], created_at: 1, updated_at: 1 },
        ],
      },
    }),
  );
  await page.route("**/api/patrols", (r: Route) =>
    r.fulfill({
      json: {
        patrols: [
          {
            id: "pt1",
            conversation_id: "c9",
            title: "Hourly incident sweep",
            conversation_title: "All-hands war room",
            cron: "0 * * * *",
            timezone: "UTC",
            status: "active",
          },
        ],
      },
    }),
  );
  await page.route("**/api/orchestrator/tasks?*", (r: Route) =>
    r.fulfill({
      json: {
        data: [{ id: "t1", title: "Nightly revenue export", prompt: "Export", status: "completed" }],
        total: 1,
        limit: 50,
        offset: 0,
      },
    }),
  );
  let own = opts.own ?? null;
  await page.route("**/api/sidebar", (r: Route) => {
    const method = r.request().method();
    if (method === "PUT") {
      own = r.request().postDataJSON() as Layout;
      puts.push(own);
      return r.fulfill({ json: { layout: own } });
    }
    if (method === "DELETE") {
      own = null;
      return r.fulfill({ json: { layout: null } });
    }
    return r.fulfill({ json: { layout: own, workspace_default: opts.workspace ?? null } });
  });
  return { puts };
}

async function shot(page: Page, name: string) {
  if (!SHOTS) return;
  await page.waitForTimeout(250);
  await page.screenshot({ path: `${SHOTS}/${name}.png` });
}

function railNames(page: Page) {
  return page.getByTestId("rail-nav").locator(":scope > a, :scope > button").allInnerTexts();
}

test.beforeEach(async ({ context, page }) => {
  await loginViaCookie(context);
  await page.setViewportSize({ width: 1360, height: 860 });
});

test("the default layout is the rail as it always was", async ({ page }) => {
  await setup(page);
  await page.goto("/chat");
  await expect(page.getByTestId("nav-to-chat")).toBeVisible();
  await expect(page.getByTestId("launchpad-button")).toBeVisible();
  await expect(page.getByTestId("nav-to-orchestrator")).toBeVisible();
  await expect(page.getByTestId("rail-shortcut")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Projects", exact: true })).toBeVisible();
  await shot(page, "01-default");
});

test("a saved layout pins shortcuts, renames, hides, reorders and marks Home", async ({ page }) => {
  await setup(page, {
    own: {
      version: 1,
      items: [
        { id: "s-board", kind: "chat", ref: "c2", title: "Revenue board", view: "dashboard" },
        { id: "s-war", kind: "chat", ref: "c9", title: "All-hands war room", icon: "activity" },
        { id: "chat", kind: "builtin", label: "Ask anything" },
        { id: "tasks", kind: "builtin", label: "Automations" },
        { id: "s-proj", kind: "project", ref: "p1", title: "Launch plan" },
        { id: "patrols", kind: "builtin", hidden: true },
      ],
      sections: [{ id: "chats" }, { id: "team" }, { id: "projects", hidden: true }],
      home: "s-board",
    },
  });
  await page.goto("/chat?c=c4");
  const nav = page.getByTestId("rail-nav");
  await expect(nav.getByText("Revenue board")).toBeVisible();
  await expect(nav.getByText("Ask anything")).toBeVisible();
  await expect(nav.getByText("Automations")).toBeVisible();
  await expect(page.getByTestId("launchpad-button")).toHaveCount(0);
  await expect(nav.getByText("(Home)")).toHaveCount(1);
  const names = await railNames(page);
  expect(names[0]).toContain("Revenue board");
  expect(names[0]).toContain("Dashboard");
  // The Projects section is hidden; the chats still list.
  await expect(page.getByRole("button", { name: "Projects", exact: true })).toHaveCount(0);
  await expect(page.getByTestId("pinned-section")).toBeVisible();
  await shot(page, "02-pinned-shortcuts");
});

test("Home opens when fleet is opened, and only then", async ({ page }) => {
  await setup(page, {
    own: {
      version: 1,
      items: [
        { id: "s-board", kind: "chat", ref: "c5", title: "Vendor comparison" },
        { id: "chat", kind: "builtin" },
        { id: "patrols", kind: "builtin" },
        { id: "tasks", kind: "builtin" },
      ],
      sections: [],
      home: "s-board",
    },
  });
  const opened = page.waitForRequest((r) => r.url().endsWith("/api/conversations/c5") && r.method() === "GET");
  await page.goto("/chat");
  await opened;
  await expect(page.getByTestId("rail-shortcut")).toHaveAttribute("aria-current", "page");
});

test("a link to a chat wins over Home", async ({ page }) => {
  await setup(page, {
    own: {
      version: 1,
      items: [{ id: "s-board", kind: "chat", ref: "c5", title: "Vendor comparison" }],
      sections: [],
      home: "s-board",
    },
  });
  const homeReads: string[] = [];
  page.on("request", (r) => {
    if (r.url().endsWith("/api/conversations/c5")) homeReads.push(r.url());
  });
  const opened = page.waitForRequest((r) => r.url().endsWith("/api/conversations/c4"));
  await page.goto("/chat?c=c4");
  await opened;
  await page.waitForTimeout(400);
  expect(homeReads).toEqual([]);
});

test("the workspace default applies to people without their own", async ({ page }) => {
  await setup(page, {
    workspace: {
      version: 1,
      items: [
        { id: "s-war", kind: "chat", ref: "c9", title: "All-hands war room", icon: "🛰️", label: "War room" },
        { id: "chat", kind: "builtin" },
        { id: "patrols", kind: "builtin" },
        { id: "tasks", kind: "builtin", hidden: true },
      ],
      sections: [{ id: "team" }, { id: "chats" }, { id: "projects", collapsed: true }],
      compact: true,
    },
  });
  await page.goto("/chat");
  const nav = page.getByTestId("rail-nav");
  await expect(nav.getByText("War room")).toBeVisible();
  await expect(page.getByTestId("nav-to-orchestrator")).toHaveCount(0);
  // Compact: no subtitles.
  await expect(nav.getByText("Talk it through")).toHaveCount(0);
  await shot(page, "03-workspace-default-team-room");
});

test("Customize: presets, adding a shortcut and renaming save to the account", async ({ page }) => {
  const { puts } = await setup(page);
  await page.goto("/chat");
  await page.getByTestId("account-menu-button").click();
  await page.getByTestId("customize-sidebar").click();
  const panel = page.getByTestId("sidebar-customizer");
  await expect(panel).toBeVisible();
  await shot(page, "04-customize-panel");

  // Add a dashboard: pick the chat, then make it open as a dashboard.
  await panel.getByTestId("sidebar-add-shortcut").click();
  await page.getByRole("option", { name: /Revenue board/ }).click();
  const row = panel.getByTestId("sidebar-editor-item").filter({ has: page.getByPlaceholder("Revenue board") });
  await row.getByRole("button", { name: "Dashboard" }).click();
  await row.getByTestId("sidebar-item-home").click();
  // The rail shows it at once, at the top, as Home.
  await expect(page.getByTestId("rail-nav").getByText("Revenue board")).toBeVisible();

  // A preset rearranges the destinations and keeps the shortcut.
  await panel.getByTestId("sidebar-preset-dashboards").click();
  await expect(page.getByTestId("launchpad-button")).toHaveCount(0);
  await expect(page.getByTestId("rail-nav").getByText("Revenue board")).toBeVisible();

  // Rename a destination.
  const chatRow = panel.getByTestId("sidebar-editor-item").filter({ has: page.getByPlaceholder("Chat", { exact: true }) });
  await chatRow.getByTestId("sidebar-item-name").fill("Ask the agent");
  await expect(page.getByTestId("nav-to-chat")).toContainText("Ask the agent");
  await shot(page, "05-customize-live-preview");

  await expect.poll(() => puts.length).toBeGreaterThan(0);
  await expect
    .poll(() => {
      const last = puts[puts.length - 1] as { items?: { kind: string; ref?: string; view?: string; label?: string }[]; home?: string };
      const board = last.items?.find((i) => i.kind === "chat" && i.ref === "c2");
      const chat = last.items?.find((i) => i.kind === "builtin" && (i as { id?: string }).id === "chat");
      return [board?.view, chat?.label, Boolean(last.home)];
    })
    .toEqual(["dashboard", "Ask the agent", true]);

  const results = await new AxeBuilder({ page })
    .include('[data-testid="sidebar-customizer"]')
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`)).toEqual([]);

  await panel.getByTestId("sidebar-done").click();
  await expect(panel).toHaveCount(0);
  await shot(page, "06-dashboards-first");
});

test("a chat's menu adds it to the sidebar", async ({ page }) => {
  const { puts } = await setup(page);
  await page.goto("/chat");
  await page.getByRole("button", { name: "Options for Vendor comparison" }).click();
  await page.getByTestId("add-to-sidebar").click();
  await expect(page.getByTestId("rail-shortcut")).toContainText("Vendor comparison");
  await expect.poll(() => puts.length).toBeGreaterThan(0);
});

test("Settings → Admin → Sidebar edits the workspace default", async ({ page }) => {
  await setup(page, { admin: true });
  const saves: Layout[] = [];
  await page.route("**/api/admin/sidebar", (r: Route) => {
    if (r.request().method() === "PUT") {
      const layout = r.request().postDataJSON() as Layout;
      saves.push(layout);
      return r.fulfill({ json: { layout, updated_by: "e2e@example.com", updated_at: 1_763_000_000 } });
    }
    return r.fulfill({ json: { layout: null, updated_by: "", updated_at: 0 } });
  });
  await page.goto("/settings/admin/sidebar");
  await expect(page.getByRole("heading", { name: "Sidebar", exact: true })).toBeVisible();
  await page.getByTestId("sidebar-preset-team").click();
  await page.getByTestId("admin-sidebar-save").click();
  await expect.poll(() => saves.length).toBe(1);
  await expect(page.getByTestId("admin-sidebar-provenance")).toContainText("e2e@example.com");
  await shot(page, "07-admin-workspace-default");
});

test("pinning is per item: one of many multiplayer chats, two of many dashboards, each renamed", async ({ page }) => {
  const many = [
    { id: "d1", title: "Revenue board", pinned: true },
    { id: "d2", title: "Pipeline board", pinned: true },
    { id: "d3", title: "Churn board", pinned: true },
    { id: "d4", title: "Hiring board", pinned: true },
    { id: "m1", title: "Incident room", multiplayer: true, user_email: "ops@example.com" },
    { id: "m2", title: "Launch war room", multiplayer: true, user_email: "pm@example.com" },
    { id: "m3", title: "Support swarm", multiplayer: true, user_email: "cs@example.com" },
  ];
  const puts: Layout[] = [];
  await mockChatBoot(page, { conversations: many });
  await page.route("**/api/me", (r: Route) => r.fulfill({ json: { email: "e2e@example.com", role: "member" } }));
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [] } }));
  await page.route("**/api/patrols", (r: Route) => r.fulfill({ json: { patrols: [] } }));
  await page.route("**/api/orchestrator/tasks?*", (r: Route) => r.fulfill({ json: { data: [], total: 0, limit: 50, offset: 0 } }));
  let own: Layout | null = null;
  await page.route("**/api/sidebar", (r: Route) => {
    if (r.request().method() === "PUT") {
      own = r.request().postDataJSON() as Layout;
      puts.push(own);
      return r.fulfill({ json: { layout: own } });
    }
    return r.fulfill({ json: { layout: own, workspace_default: null } });
  });
  await page.goto("/chat");

  // One multiplayer chat, straight from its Team chats row.
  const incident = page.getByTestId("team-shared-conv-row").filter({ hasText: "Incident room" });
  await incident.hover();
  await page.getByRole("button", { name: "Add Incident room to the sidebar" }).click();
  await expect(page.getByTestId("rail-shortcut")).toHaveCount(1);

  // Two of the four dashboards, from the Customize panel's picker.
  await page.getByTestId("account-menu-button").click();
  await page.getByTestId("customize-sidebar").click();
  const panel = page.getByTestId("sidebar-customizer");
  for (const name of ["Revenue board", "Churn board"]) {
    await panel.getByTestId("sidebar-add-shortcut").click();
    await page.getByRole("option", { name: new RegExp(name) }).click();
    const row = panel.getByTestId("sidebar-editor-item").filter({ has: page.getByPlaceholder(name) });
    await row.getByRole("button", { name: "Dashboard" }).click();
  }
  // Each gets a name of its own.
  await panel
    .getByTestId("sidebar-editor-item")
    .filter({ has: page.getByPlaceholder("Incident room") })
    .getByTestId("sidebar-item-name")
    .fill("🚨 On-call");
  await panel
    .getByTestId("sidebar-editor-item")
    .filter({ has: page.getByPlaceholder("Revenue board") })
    .getByTestId("sidebar-item-name")
    .fill("Revenue");
  // The rest of the team's chats stay where they were, unpinned.
  const rail = page.getByTestId("rail-nav");
  await expect(page.getByTestId("rail-shortcut")).toHaveCount(3);
  await expect(rail.getByText("🚨 On-call")).toBeVisible();
  await expect(rail.getByText("Revenue", { exact: true })).toBeVisible();
  await expect(rail.getByText("Churn board")).toBeVisible();
  await expect(rail.getByText("Launch war room")).toHaveCount(0);
  await expect(rail.getByText("Pipeline board")).toHaveCount(0);
  await shot(page, "08-granular-editing");
  await panel.getByTestId("sidebar-done").click();
  await shot(page, "09-granular-result");
  await expect.poll(() => (puts[puts.length - 1]?.items as unknown[] | undefined)?.length).toBe(6);
});

test("a workspace pin this account cannot open is hidden on every page", async ({ page }) => {
  await setup(page, {
    workspace: {
      version: 1,
      items: [
        { id: "s-war", kind: "chat", ref: "c9", title: "All-hands war room" },
        { id: "s-secret", kind: "chat", ref: "admins-private", title: "Admin's private chat" },
        { id: "s-gone", kind: "project", ref: "p-gone", title: "Someone else's project" },
      ],
      sections: [],
    },
  });
  for (const path of ["/chat", "/settings"]) {
    await page.goto(path);
    const rail = page.getByTestId("rail-nav");
    await expect(rail.getByText("All-hands war room")).toBeVisible();
    await expect(rail.getByText("Admin's private chat")).toHaveCount(0);
    await expect(rail.getByText("Someone else's project")).toHaveCount(0);
  }
});

test("Team chats stay visible when Chats starts folded", async ({ page }) => {
  await setup(page, {
    own: { version: 1, items: [], sections: [{ id: "projects" }, { id: "chats", collapsed: true }, { id: "team" }] },
  });
  await page.goto("/chat");
  await expect(page.getByRole("button", { name: "Chats", exact: true })).toHaveAttribute("aria-expanded", "false");
  await expect(page.getByTestId("team-chats-section")).toBeVisible();
  await expect(page.getByTestId("team-shared-conv-row")).toContainText("All-hands war room");
});

test("a read-only Viewer can look at the panel but not change the sidebar", async ({ page }) => {
  const { puts } = await setup(page, { role: "viewer" });
  await page.goto("/chat");
  await page.getByRole("button", { name: "Options for Vendor comparison" }).click();
  await expect(page.getByTestId("add-to-sidebar")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await page.getByTestId("account-menu-button").click();
  await page.getByTestId("customize-sidebar").click();
  const panel = page.getByTestId("sidebar-customizer");
  await expect(panel.getByTestId("sidebar-read-only")).toBeVisible();
  await expect(panel.getByTestId("sidebar-add-shortcut")).toBeDisabled();
  await expect(panel.getByTestId("sidebar-preset-chat")).toBeDisabled();
  expect(puts).toEqual([]);
});

test("the sidebar over a dashboard is a modal drawer: Escape closes it, not the view", async ({ page }) => {
  await setup(page);
  await page.goto("/chat?c=c2");
  await page.getByTestId("dashboard-enter").click();
  await expect(page.getByTestId("dashboard-hud")).toBeVisible();
  const opener = page.getByTestId("dashboard-sidebar-button");
  await opener.click();
  const rail = page.getByTestId("rail-nav");
  await expect(rail).toBeVisible();
  await expect.poll(() => page.evaluate(() => document.activeElement?.closest("[data-dashboard-rail-wrapper]") !== null)).toBe(true);
  await page.keyboard.press("Escape");
  await expect(rail).toBeHidden();
  await expect(page.getByTestId("dashboard-hud")).toBeVisible();
  await expect(opener).toBeFocused();
});

test("the admin page never saves over a workspace sidebar it could not load", async ({ page }) => {
  await setup(page, { admin: true });
  let fail = true;
  await page.route("**/api/admin/sidebar", (r: Route) =>
    fail
      ? r.fulfill({ status: 503, body: "unavailable" })
      : r.fulfill({ json: { layout: null, updated_by: "", updated_at: 0 } }),
  );
  await page.goto("/settings/admin/sidebar");
  await expect(page.getByTestId("admin-sidebar-retry")).toBeVisible();
  await expect(page.getByTestId("sidebar-preset-team")).toBeDisabled();
  await expect(page.getByTestId("admin-sidebar-save")).toBeDisabled();
  fail = false;
  await page.getByTestId("admin-sidebar-retry").click();
  await expect(page.getByTestId("sidebar-preset-team")).toBeEnabled();
  await page.getByTestId("sidebar-preset-team").click();
  await expect(page.getByTestId("admin-sidebar-save")).toBeEnabled();
});

test("Start from my sidebar never copies a sidebar that failed to load", async ({ page }) => {
  await setup(page, { admin: true });
  let ownFails = true;
  await page.route("**/api/sidebar", (r: Route) =>
    ownFails
      ? r.fulfill({ status: 502, body: "down" })
      : r.fulfill({
          json: {
            layout: { version: 1, items: [{ id: "tasks", kind: "builtin", label: "Ops" }], sections: [] },
            workspace_default: null,
          },
        }),
  );
  await page.route("**/api/admin/sidebar", (r: Route) =>
    r.fulfill({
      json: {
        layout: { version: 1, items: [{ id: "chat", kind: "builtin", label: "Team default" }], sections: [] },
        updated_by: "boss@example.com",
        updated_at: 1_763_000_000,
      },
    }),
  );
  await page.goto("/settings/admin/sidebar");
  const names = page.getByTestId("admin-sidebar-editor").getByTestId("sidebar-item-name");
  await expect(names.first()).toHaveValue("Team default");
  await page.getByTestId("admin-sidebar-copy-mine").click();
  await expect(page.getByText("Couldn't load your sidebar, so nothing was copied. Try again.")).toBeVisible();
  await expect(names.first()).toHaveValue("Team default");
  await expect(page.getByTestId("admin-sidebar-save")).toBeDisabled();
  // Once the read recovers, the copy is retried and lands.
  ownFails = false;
  await page.getByTestId("admin-sidebar-copy-mine").click();
  await expect(names.first()).toHaveValue("Ops");
});

test("your own sidebar can pin a private chat; the workspace sidebar offers only what the team can see", async ({ page }) => {
  await setup(page, { admin: true });
  // Personal: a private chat (c5) is offered and pins.
  await page.goto("/chat");
  await page.getByTestId("account-menu-button").click();
  await page.getByTestId("customize-sidebar").click();
  const panel = page.getByTestId("sidebar-customizer");
  await panel.getByTestId("sidebar-add-shortcut").click();
  await expect(page.getByRole("option", { name: /Vendor comparison/ })).toBeVisible();
  await page.getByRole("option", { name: /Vendor comparison/ }).click();
  await expect(page.getByTestId("rail-nav").getByText("Vendor comparison")).toBeVisible();

  // Workspace: the same private chat is not offered; the multiplayer chat is.
  await page.route("**/api/admin/sidebar", (r: Route) =>
    r.fulfill({
      json: {
        layout: { version: 1, items: [{ id: "s-p", kind: "chat", ref: "c5", title: "Vendor comparison" }], sections: [] },
        updated_by: "e2e@example.com",
        updated_at: 1_763_000_000,
      },
    }),
  );
  await page.goto("/settings/admin/sidebar");
  const editor = page.getByTestId("admin-sidebar-editor");
  // A private pin already in the workspace default is flagged.
  await expect(editor.getByTestId("sidebar-item-not-shared")).toHaveCount(1);
  await editor.getByTestId("sidebar-add-shortcut").click();
  await expect(page.getByRole("option", { name: /All-hands war room/ }).first()).toBeVisible();
  await expect(page.getByRole("option", { name: /Vendor comparison/ })).toHaveCount(0);
  await expect(page.getByRole("option", { name: /Draft launch email/ })).toHaveCount(0);
});

test("the sidebar cannot be changed until the saved one has loaded", async ({ page }) => {
  const { puts } = await setup(page);
  let fail = true;
  await page.route("**/api/sidebar", (r: Route) =>
    fail ? r.fulfill({ status: 502, body: "down" }) : r.fulfill({ json: { layout: null, workspace_default: null } }),
  );
  await page.goto("/chat");
  await page.getByRole("button", { name: "Options for Vendor comparison" }).click();
  await expect(page.getByTestId("add-to-sidebar")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await page.getByTestId("account-menu-button").click();
  await page.getByTestId("customize-sidebar").click();
  const panel = page.getByTestId("sidebar-customizer");
  await expect(panel.getByTestId("sidebar-unavailable")).toBeVisible();
  await expect(panel.getByTestId("sidebar-preset-chat")).toBeDisabled();
  expect(puts).toEqual([]);
  fail = false;
  await panel.getByRole("button", { name: "Try again" }).click();
  await expect(panel.getByTestId("sidebar-preset-chat")).toBeEnabled();
});

test("Analytics is never pinned for the whole team", async ({ page }) => {
  await setup(page, { admin: true });
  await page.route("**/api/admin/sidebar", (r: Route) => r.fulfill({ json: { layout: null, updated_by: "", updated_at: 0 } }));
  await page.goto("/settings/admin/sidebar");
  await page.getByTestId("admin-sidebar-editor").getByTestId("sidebar-add-shortcut").click();
  await expect(page.getByRole("option", { name: /Guides/ })).toBeVisible();
  await expect(page.getByRole("option", { name: /Analytics/ })).toHaveCount(0);
});

test("a workspace pin to a chat that is plainly here shows even when the team list fails", async ({ page }) => {
  await setup(page, {
    workspace: { version: 1, items: [{ id: "s-own", kind: "chat", ref: "c4", label: "Launch email" }], sections: [] },
  });
  await page.route("**/api/conversations?*", (r: Route) => {
    const url = new URL(r.request().url());
    if (url.searchParams.get("scope") === "team") return r.fulfill({ status: 500, body: "down" });
    if (url.searchParams.get("archived") === "true") return r.fulfill({ json: { conversations: [] } });
    return r.fulfill({ json: { conversations: [] } });
  });
  await page.goto("/chat");
  await expect(page.getByTestId("rail-nav").getByText("Launch email")).toBeVisible();
});

test("a pin to a deleted project reads as not available once projects have loaded", async ({ page }) => {
  await setup(page, {
    own: { version: 1, items: [{ id: "s-gone", kind: "project", ref: "p-gone", title: "Old project" }], sections: [] },
  });
  await page.route("**/api/projects", (r: Route) => r.fulfill({ json: { projects: [] } }));
  await page.goto("/chat");
  await expect(page.getByTestId("rail-shortcut")).toContainText("Not available to you");
});

test("the workspace editor flags an archived chat pinned there, and never copies Analytics", async ({ page }) => {
  await setup(page, { admin: true });
  await page.route("**/api/conversations?*", (r: Route) => {
    const url = new URL(r.request().url());
    if (url.searchParams.get("archived") === "true")
      return r.fulfill({ json: { conversations: [{ id: "c-old", title: "Old board", archived_at: 1 }] } });
    return r.fulfill({ json: { conversations: [] } });
  });
  await page.route("**/api/admin/sidebar", (r: Route) =>
    r.fulfill({
      json: {
        layout: { version: 1, items: [{ id: "s-old", kind: "chat", ref: "c-old", title: "Old board" }], sections: [] },
        updated_by: "e2e@example.com",
        updated_at: 1_763_000_000,
      },
    }),
  );
  await page.route("**/api/sidebar", (r: Route) =>
    r.fulfill({
      json: { layout: { version: 1, items: [{ id: "s-a", kind: "page", ref: "analytics" }], sections: [] }, workspace_default: null },
    }),
  );
  await page.goto("/settings/admin/sidebar");
  const editor = page.getByTestId("admin-sidebar-editor");
  await expect(editor.getByTestId("sidebar-item-not-shared")).toHaveCount(1);
  await page.getByTestId("admin-sidebar-copy-mine").click();
  await expect(editor.getByTestId("sidebar-item-not-shared")).toHaveCount(0);
  await expect(editor.getByPlaceholder("Analytics")).toHaveCount(0);
});

test("a dashboard shortcut's link opens as a dashboard, even in a new tab", async ({ page }) => {
  await setup(page, {
    own: { version: 1, items: [{ id: "s-board", kind: "chat", ref: "c2", title: "Revenue board", view: "dashboard" }], sections: [] },
  });
  await page.goto("/chat?c=c4");
  const href = await page.getByTestId("rail-shortcut").getAttribute("href");
  expect(href).toBe("/chat?c=c2&view=dashboard");
  // Following the raw URL (a new tab, a copied link) honours the view too.
  await page.goto(href!);
  await expect(page.getByTestId("dashboard-hud")).toBeVisible();
  await expect(page).toHaveURL(/\/chat$/);
});
