import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// The ?connector=<name> deep link (docs and the browserbase skill point users
// here): landing on /settings/connections?connector=browserbase must filter
// the directory to the Browserbase card with its guided key form already open
// and focused, and the paste-and-add flow must POST the key together with the
// manifest's api_key_query — the Browserbase hosted server takes the key as a
// query parameter, so an add without it fails its validation probe.

const catalog = {
  bundled: [],
  remote_mcp_enabled: true,
  third_party: [
    {
      name: "browserbase",
      display_name: "Browserbase",
      description:
        "Cloud browser automation via Stagehand — navigate, act, extract.",
      url: "https://mcp.browserbase.com/mcp?keepAlive=true",
      vendor: "Browserbase, Inc.",
      provenance: "official",
      category: "web-search",
      auth: "api_key",
      api_key_query: "browserbaseApiKey",
      setup_hint: "In the Browserbase dashboard, copy your API key.",
      setup_url: "https://www.browserbase.com/overview",
      featured: true,
    },
    {
      name: "exa",
      display_name: "Exa",
      description: "Web search for AIs.",
      url: "https://mcp.exa.ai/mcp",
      vendor: "Exa Labs, Inc.",
      provenance: "official",
      category: "web-search",
    },
  ],
};

async function mockConnectionsBoot(page: Page) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) =>
    r.fulfill({ json: { build_id: "test" } }),
  );
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/admin/settings", (r: Route) =>
    r.fulfill({ status: 403, body: "forbidden — not an admin" }),
  );
  await page.route("**/api/mcp-servers", (r: Route) =>
    r.fulfill({ json: { servers: [] } }),
  );
  await page.route("**/api/connector-prefs", (r: Route) =>
    r.fulfill({ json: { prefs: [] } }),
  );
  await page.route("**/api/mcp-catalog", (r: Route) => r.fulfill({ json: catalog }));
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("?connector=browserbase lands one paste away from connected", async ({
  page,
}) => {
  await mockConnectionsBoot(page);
  let addBody: Record<string, unknown> | null = null;
  await page.route("**/api/remote-mcp-servers", async (r: Route) => {
    if (r.request().method() === "POST") {
      addBody = r.request().postDataJSON() as Record<string, unknown>;
      await r.fulfill({ json: { id: "srv1", tool_count: 6 } });
      return;
    }
    await r.fulfill({ json: { servers: [] } });
  });

  await page.goto("/settings/connections?connector=browserbase");

  // The directory is filtered to the linked entry and its form is open.
  const form = page.getByTestId("dir-form-browserbase");
  await expect(form).toBeVisible({ timeout: 15_000 });
  await expect(page.getByLabel("Search connector directory")).toHaveValue(
    "browserbase",
  );
  await expect(page.getByTestId("dir-card-exa")).toHaveCount(0);
  // The one-shot param is stripped from the URL once consumed.
  await expect(page).toHaveURL(/\/settings\/connections$/);

  // The key field holds focus: paste and add without another click.
  const key = form.getByPlaceholder(
    "paste your key (stored encrypted, never shown again)",
  );
  await expect(key).toBeFocused();
  await key.fill("bb_e2e_key");
  await page.getByTestId("dir-form-add-browserbase").click();

  await expect
    .poll(() => addBody, { message: "add POST should have fired" })
    .not.toBeNull();
  expect(addBody).toMatchObject({
    name: "browserbase",
    url: "https://mcp.browserbase.com/mcp?keepAlive=true",
    auth: "api_key",
    api_key: "bb_e2e_key",
    api_key_query: "browserbaseApiKey",
  });
});
