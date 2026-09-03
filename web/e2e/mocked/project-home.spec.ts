import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot } from "./_mocks";

// Mocked e2e for the project home (#509 follow-up): clicking a project's name
// in the rail opens its home — title, this member's chats, a Sources panel
// (workspace files), and the instructions editor. The rail kebab's "Project
// settings…" opens the same home with the per-project settings dialog. All
// /api/* calls are intercepted.

const PROJECT = {
  id: "p-growth",
  owner_email: "e2e@example.com",
  name: "Growth experiments",
  instructions: "Always cite the experiment doc.",
  mcp_servers: [],
  created_at: 1_700_000_000,
  updated_at: 1_700_000_500,
};

async function mockProjectHome(page: Page) {
  await page.route("**/api/projects", (r: Route) =>
    r.fulfill({ json: { projects: [PROJECT] } }),
  );
  await page.route("**/api/projects/p-growth/conversations", (r: Route) =>
    r.fulfill({
      json: {
        conversations: [
          {
            id: "c1",
            title: "Landing page A/B",
            updated_at: 1_700_000_400,
            preview: "You: what did variant B do to signups?",
          },
        ],
      },
    }),
  );
  await page.route("**/api/projects/p-growth/files", (r: Route) =>
    r.fulfill({
      json: {
        files: [
          {
            conversation_id: "c1",
            conversation_title: "Landing page A/B",
            path: "plots/uplift.png",
            name: "uplift.png",
            size: 20480,
            modified_at: 1_700_000_400,
          },
        ],
        truncated: false,
      },
    }),
  );
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("project name opens the home: chats, sources, instructions", async ({
  page,
}) => {
  await mockProjectHome(page);
  await mockChatBoot(page, {
    conversations: [
      { id: "c1", title: "Landing page A/B", project_id: "p-growth" },
      { id: "c9", title: "Unrelated chat" },
    ],
  });
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page
    .getByRole("button", { name: "Open project Growth experiments" })
    .click();
  const home = page.getByTestId("project-home");
  await expect(home).toBeVisible();
  await expect(
    home.getByRole("heading", { name: "Growth experiments" }),
  ).toBeVisible();

  // This member's chats in the project — and only those — each with its
  // 1–2 line history preview. Named exactly: a Sources row also mentions the
  // chat it came from, and both are buttons now that a download reports its
  // failure in-app instead of opening a tab of server text.
  await expect(
    home.getByRole("button", { name: "Landing page A/B You: what did variant B do to signups?" }),
  ).toBeVisible();
  await expect(
    home.getByText("You: what did variant B do to signups?"),
  ).toBeVisible();
  await expect(home.getByText("Unrelated chat")).toHaveCount(0);

  // Sources render from the files endpoint with size + source chat.
  await expect(home.getByText("uplift.png")).toBeVisible();
  await expect(home.getByText("20.0 KB")).toBeVisible();

  // Instructions editor carries the saved value (owner → editable textarea).
  await expect(home.getByLabel("Project instructions")).toHaveValue(
    "Always cite the experiment doc.",
  );

  // Back returns to the chat view.
  await home.getByRole("button", { name: "Back to chat" }).click();
  await expect(page.getByTestId("project-home")).toHaveCount(0);
});

test("editing instructions PATCHes just the instructions", async ({ page }) => {
  let patched = "";
  await mockProjectHome(page);
  await page.route("**/api/projects/p-growth", (r: Route) => {
    if (r.request().method() === "PATCH") {
      patched = r.request().postData() ?? "";
      return r.fulfill({ json: { ...PROJECT, instructions: "New rules." } });
    }
    return r.fulfill({ json: PROJECT });
  });
  await mockChatBoot(page, { conversations: [] });
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page
    .getByRole("button", { name: "Open project Growth experiments" })
    .click();
  const box = page.getByLabel("Project instructions");
  await box.fill("New rules.");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect
    .poll(() => patched, { timeout: 5_000 })
    .toContain('"instructions":"New rules."');
});

test("the kebab's Project settings… opens the per-project dialog", async ({
  page,
}) => {
  await mockProjectHome(page);
  await mockChatBoot(page, { conversations: [] });
  await page.goto("/chat");
  await page
    .getByRole("heading", { name: /what can i help with/i })
    .waitFor({ timeout: 15_000 });

  await page
    .getByRole("button", { name: "Project options for Growth experiments" })
    .click();
  await page.getByRole("menuitem", { name: "Project settings…" }).click();

  const dialog = page.getByRole("dialog", {
    name: "Settings for Growth experiments",
  });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Project name")).toHaveValue(
    "Growth experiments",
  );
  await expect(
    dialog.getByRole("button", { name: "Delete project" }),
  ).toBeVisible();
});
