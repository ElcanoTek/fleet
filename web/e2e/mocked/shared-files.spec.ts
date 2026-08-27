import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { loginViaCookie } from "./_session";

// Mocked e2e for Settings → Shared files (the cross-chat shared file
// library): the page renders the folder-grouped library from GET
// /api/shared-files for an admin (upload + manage controls visible), and a
// member — admin probe 403 — still gets the same list read-only with
// download links. Role comes from the /api/admin/settings probe, exactly as
// the settings sub-nav resolves it.

type SharedFile = {
  id: string;
  name: string;
  folder: string;
  description: string;
  size_bytes: number;
  uploaded_by?: string;
  created_at: number;
  updated_at: number;
};

const LIBRARY: { files: SharedFile[]; total_bytes: number; max_total_bytes: number } = {
  files: [
    {
      id: "f-report",
      name: "quarterly-report.pdf",
      folder: "",
      description: "Q2 numbers",
      size_bytes: 1024 * 1024,
      uploaded_by: "admin@example.com",
      created_at: 1_755_000_000,
      updated_at: 1_755_000_000,
    },
    {
      id: "f-playbook",
      name: "playbook.md",
      folder: "docs",
      description: "",
      size_bytes: 512 * 1024,
      uploaded_by: "admin@example.com",
      created_at: 1_755_000_000,
      updated_at: 1_755_000_000,
    },
  ],
  total_bytes: 1536 * 1024,
  max_total_bytes: 100 * 1024 * 1024,
};

async function mockSharedFiles(page: Page, opts: { admin: boolean }) {
  await page.route("**/api/session", (r: Route) =>
    r.fulfill({ json: { email: "e2e@example.com" } }),
  );
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/client-config", (r: Route) => r.fulfill({ json: {} }));
  await page.route("**/api/push/vapid-public-key", (r: Route) =>
    r.fulfill({ status: 501, json: { error: "not configured" } }),
  );
  // The admin probe the page (and sub-nav) folds into a role.
  await page.route("**/api/admin/settings", (r: Route) =>
    opts.admin
      ? r.fulfill({ json: { settings: [] } })
      : r.fulfill({ status: 403, body: "forbidden — not an admin" }),
  );
  await page.route("**/api/shared-files", (r: Route) => r.fulfill({ json: LIBRARY }));
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

test("an admin sees the grouped library with upload and manage controls", async ({ page }) => {
  await mockSharedFiles(page, { admin: true });
  await page.goto("/settings/shared-files");

  const panel = page.getByTestId("shared-files-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("quarterly-report.pdf");
  await expect(panel).toContainText("Library root");
  await expect(panel).toContainText("docs");
  await expect(page.getByTestId("usage-meter")).toHaveText("1.5 MB of 100.0 MB used");

  // Admin affordances: the upload form and per-row manage controls.
  await expect(page.getByTestId("upload-files-input")).toBeVisible();
  await expect(page.getByTestId("edit-f-report")).toBeVisible();
  await expect(page.getByTestId("delete-f-report")).toBeVisible();
});

test("a member (admin probe 403) still gets the read-only library", async ({ page }) => {
  await mockSharedFiles(page, { admin: false });
  await page.goto("/settings/shared-files");

  const panel = page.getByTestId("shared-files-panel");
  await expect(panel).toBeVisible({ timeout: 15_000 });
  await expect(panel).toContainText("quarterly-report.pdf");
  await expect(panel).toContainText("playbook.md");
  await expect(
    page.getByRole("link", { name: "Download quarterly-report.pdf" }),
  ).toHaveAttribute("href", "/api/shared-files/f-report/download");

  // Read-only: no upload form, no per-row manage controls.
  await expect(page.getByTestId("upload-files-input")).toHaveCount(0);
  await expect(page.getByTestId("edit-f-report")).toHaveCount(0);
  await expect(page.getByTestId("delete-f-report")).toHaveCount(0);

  // The section itself stays in the member's sub-nav.
  const nav = page.getByRole("navigation", { name: "Settings sections" });
  await expect(nav.getByRole("link", { name: "Shared files" })).toHaveAttribute(
    "aria-current",
    "page",
  );
});
