import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockChatBoot, mockShellBasics } from "./_mocks";
import { loginViaCookie } from "./_session";

// A name nobody trimmed must not widen the screen. Each of these screens once
// did exactly that, one at a time, when a single value had no space to break
// at: the row's grid grew to fit the word and took the page with it. The fix is
// two stylesheet defaults (globals.css, beside `:where(.grid)`); this spec is
// the check that they still hold on a phone and on a desktop. When you add a
// screen that shows a value that comes from data, add it here.

const TOKEN =
  "AVeryLongUnbrokenIdentifierWithoutAnySpacesThatKeepsGoingAndGoingAndGoingUntilItIsWiderThanAnyCardOrColumn";

const WIDTHS = [
  { name: "small phone", width: 320, height: 640 },
  { name: "phone", width: 390, height: 844 },
  { name: "tablet", width: 768, height: 900 },
  { name: "desktop", width: 1280, height: 800 },
];

// Everything on screen must end inside the window, unless it sits in a region
// that scrolls sideways on purpose (a table, a code block) or is clipped by
// one. Returns a description of each element that does not.
async function sidewaysOverflow(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    const vw = document.documentElement.clientWidth;
    if (document.documentElement.scrollWidth > vw + 1) {
      out.push(`the page scrolls sideways (${document.documentElement.scrollWidth} > ${vw})`);
    }
    for (const el of Array.from(document.body.querySelectorAll<HTMLElement>("*"))) {
      const box = el.getBoundingClientRect();
      if (box.width === 0 || box.height === 0 || box.right <= vw + 1) continue;
      let contained = false;
      for (let up = el.parentElement; up && up !== document.body; up = up.parentElement) {
        const style = getComputedStyle(up);
        if (style.overflowX !== "visible" || style.position === "fixed") {
          contained = up.getBoundingClientRect().right <= vw + 1;
          break;
        }
      }
      if (contained) continue;
      out.push(`${el.tagName.toLowerCase()}.${String(el.className).slice(0, 60)} ends at ${Math.round(box.right)} of ${vw}`);
      if (out.length >= 8) break;
    }
    return out;
  });
}

// A container that was not meant to scroll sideways but now does, because one
// of its rows grew wider than it.
async function pannedContainers(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    for (const el of Array.from(document.querySelectorAll<HTMLElement>("main, main *, aside, aside *, [role='dialog'] *"))) {
      if (el.scrollWidth <= el.clientWidth + 1 || el.clientWidth === 0) continue;
      const style = getComputedStyle(el);
      if (style.overflowX !== "auto" && style.overflowX !== "scroll") continue;
      // The box that wraps a table, and a code block itself, scroll sideways
      // by design. A page that merely contains one somewhere does not.
      if (el.matches("pre") || el.closest("pre, table") || el.querySelector(":scope > table, :scope > pre")) continue;
      out.push(`${el.tagName.toLowerCase()}.${String(el.className).slice(0, 60)} scrolls sideways by ${el.scrollWidth - el.clientWidth}px`);
    }
    return out;
  });
}

// A button, link or field pushed out of a box that clips its overflow: the
// page still "fits", but the control cannot be seen or pressed.
async function clippedControls(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    const controls = document.querySelectorAll<HTMLElement>(
      "main button, main a[href], main input, main select, main textarea, [role='dialog'] button",
    );
    for (const el of Array.from(controls)) {
      if (!el.checkVisibility({ checkVisibilityCSS: true })) continue;
      const box = el.getBoundingClientRect();
      if (box.width === 0) continue;
      for (let up = el.parentElement; up && up !== document.body; up = up.parentElement) {
        const overflow = getComputedStyle(up).overflowX;
        if (overflow === "visible") continue;
        // A box that scrolls sideways shows the control when scrolled.
        if (overflow !== "hidden" && overflow !== "clip") break;
        const edge = up.getBoundingClientRect().right;
        if (box.right > edge + 1) {
          out.push(`${el.tagName.toLowerCase()} "${(el.innerText || el.getAttribute("aria-label") || "").slice(0, 30)}" is cut off by ${Math.round(box.right - edge)}px`);
        }
        break;
      }
    }
    return out;
  });
}

// Polled, because a drawer or a rail that is still sliding open is briefly
// wider than its box. A layout that really does not fit never settles, and
// still fails when the time runs out.
async function expectFits(page: Page) {
  await expect
    .poll(
      async () => [
        ...(await sidewaysOverflow(page)),
        ...(await pannedContainers(page)),
        ...(await clippedControls(page)),
      ],
      { timeout: 5_000 },
    )
    .toEqual([]);
}

test.beforeEach(async ({ context }) => {
  await loginViaCookie(context);
});

for (const vp of WIDTHS) {
  test.describe(`long values at ${vp.name} width`, () => {
    test.use({ viewport: { width: vp.width, height: vp.height } });

    test("System checks: a long check name and a long fix command", async ({ page }) => {
      await mockShellBasics(page, { admin: true });
      await page.route("**/api/admin/doctor**", (r: Route) =>
        r.fulfill({
          json: {
            generated_at: "2026-07-22T12:00:00Z",
            duration_ms: 850,
            deep: false,
            healthy: false,
            summary: { ok: 1, warn: 0, fail: 1, skip: 0 },
            checks: [
              { name: `unit ${TOKEN}`, status: "ok", detail: `reachable via ${TOKEN}` },
              { name: `disk: ${TOKEN}`, status: "fail", detail: TOKEN, fix: `sudo fleet cleanup --path=/var/lib/${TOKEN}` },
            ],
          },
        }),
      );
      await page.goto("/settings/admin/doctor");
      await expect(page.getByText(`unit ${TOKEN}`)).toBeVisible();
      await expectFits(page);
    });

    test("Prompt library: a long prompt name, owner and description", async ({ page }) => {
      await page.route("**/api/**", (route: Route) => route.fulfill({ json: {} }));
      await mockShellBasics(page);
      await page.route("**/api/orchestrator/prompts", (r: Route) =>
        r.fulfill({
          json: [
            {
              id: "p1",
              name: `Weekly_${TOKEN}`,
              description: TOKEN,
              content: `Summarise ${TOKEN}`,
              source: "workspace",
              visibility: "workspace",
              read_only: false,
              owned_by_caller: true,
              owner_username: `owner.${TOKEN}@example.com`,
            },
            {
              id: "p2",
              name: `Deploy_${TOKEN}`,
              description: TOKEN,
              content: TOKEN,
              source: "git",
              visibility: "workspace",
              read_only: true,
              owned_by_caller: false,
              path: `prompts/${TOKEN}.yaml`,
            },
          ],
        }),
      );
      await page.goto("/settings/prompts");
      await expect(page.getByText(`Weekly_${TOKEN}`).first()).toBeVisible();
      await expectFits(page);
    });

    test("Personas: a long persona name, with a proposed edit to another", async ({ page }) => {
      await page.route("**/api/**", (route: Route) => route.fulfill({ json: {} }));
      await mockShellBasics(page);
      const persona = {
        id: "p1",
        name: `twin-${TOKEN}`,
        ref: `user:twin-${TOKEN}`,
        display_name: `Twin_${TOKEN}`,
        description: TOKEN,
        instructions: `Lead with ${TOKEN}`,
        base_persona: "",
        kind: "twin",
        autonomy: "advise",
        status: "active",
        patrols: [{ title: `Briefing_${TOKEN}`, cron: "0 8 * * 1-5", timezone: "UTC", prompt: TOKEN }],
      };
      await page.route("**/api/user-personas", (r: Route) =>
        r.fulfill({
          json: {
            personas: [
              persona,
              { ...persona, id: "p2", name: "deal-watch", display_name: "Deal Watch", status: "proposed", replaces_name: `twin-${TOKEN}` },
            ],
            workspace_personas: ["assistant"],
            default: "assistant",
          },
        }),
      );
      await page.goto("/settings/personas");
      await expect(page.getByText(`Twin_${TOKEN}`).first()).toBeVisible();
      await expectFits(page);
    });

    test("Chat: a long chat title in the sidebar", async ({ page }) => {
      await mockChatBoot(page, {
        conversations: [
          { id: "c1", title: `Report_${TOKEN}` },
          { id: "c2", title: `Second ${TOKEN}`, labels: [`label-${TOKEN}`] },
        ],
      });
      await page.goto("/chat");
      await expect(page.getByRole("textbox").first()).toBeVisible({ timeout: 20_000 });
      const title = page.getByText(`Report_${TOKEN}`).first();
      // The list arrives after the composer; wait for the row before asking
      // whether it is on screen.
      await expect(title).toBeAttached({ timeout: 15_000 });
      if (!(await title.isVisible())) {
        // Below lg the chat list is a drawer; open it so its rows are laid out.
        await page.getByRole("button", { name: /open sidebar|expand sidebar/i }).first().click();
      }
      await expect(page.getByText(`Report_${TOKEN}`).first()).toBeVisible();
      await expectFits(page);
    });
  });
}
