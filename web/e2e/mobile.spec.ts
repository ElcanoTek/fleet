import { test, expect } from "./fixtures";

// Mobile smoke — all tests run under the "mobile" Playwright project
// (iPhone 13 device descriptor). Covers the flows most likely to break
// on small screens: login form layout, the hamburger-activated sidebar
// overlay, and the composer's sticky position over a streaming reply.

test.describe("mobile viewport", () => {
  test("login form fits the viewport and lands on chat", async ({ page, login }) => {
    await login();
    await expect(page).toHaveURL("/");

    // Composer should be visible within the initial viewport (no h-scroll).
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await expect(composer).toBeVisible();

    const box = await composer.boundingBox();
    expect(box).not.toBeNull();
    if (box) {
      const viewport = page.viewportSize();
      expect(viewport).not.toBeNull();
      if (viewport) {
        expect(box.x).toBeGreaterThanOrEqual(0);
        expect(box.x + box.width).toBeLessThanOrEqual(viewport.width + 1);
      }
    }
  });

  test("hamburger opens + closes the sidebar overlay", async ({ page, login }) => {
    await login();

    // Sidebar starts off-screen (translate-x-full) — any sidebar-only text
    // (the Elcano brand inside aside) is not visible.
    const sidebar = page.locator("aside").first();
    await expect(sidebar).toHaveClass(/-translate-x-full/);

    await page.getByRole("button", { name: /open sidebar/i }).click();
    await expect(sidebar).toHaveClass(/translate-x-0/);
    await expect(sidebar.getByText(/conversations/i)).toBeVisible();

    // Close with the X button inside the sidebar.
    await sidebar.getByRole("button", { name: /close sidebar/i }).first().click();
    await expect(sidebar).toHaveClass(/-translate-x-full/);
  });

  test("sending a message works on mobile and the composer stays visible", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("mobile smoke");
    await composer.press("Enter");

    await expect(page.getByText(/Mock reply to:\s*mobile smoke/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Composer is still on screen after the reply renders.
    await expect(composer).toBeVisible();
  });

  test("Edit is reachable without hover (touch affordance visible)", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("original");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*original/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // The Edit action is an always-visible in-flow text button (mirrors
    // the assistant Copy / Regenerate footer), so it's reachable without
    // hover at every viewport — including this touch one.
    const editBtn = page.getByRole("button", { name: /^Edit$/ });
    await expect(editBtn).toBeVisible();
    await editBtn.tap();

    const editArea = page.locator("textarea").first();
    await editArea.fill("edited via tap");
    await page.getByRole("button", { name: /^Resend$/i }).tap();

    await expect(page.getByText(/Mock reply to:\s*edited via tap/i).first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("Copy on assistant message is tappable", async ({ page, login }) => {
    await login();
    // Clipboard permission — Playwright grants it per context.
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);

    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("hello mobile");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*hello mobile/i).first()).toBeVisible({
      timeout: 15_000,
    });

    const copyBtn = page.getByRole("button", { name: /^Copy$/ });
    await expect(copyBtn).toBeVisible();
    await copyBtn.tap();
    await expect(page.getByRole("button", { name: /^Copied$/ })).toBeVisible({
      timeout: 5_000,
    });
  });

  test("Pin is reachable without hover", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("pin target");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*pin target/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Open the sidebar.
    await page.getByRole("button", { name: /open sidebar/i }).tap();
    const pinBtn = page.getByRole("button", { name: /^Pin pin target$/ });
    await expect(pinBtn).toBeVisible();
    await pinBtn.tap();
    await expect(page.getByRole("button", { name: /^Unpin pin target$/ })).toBeVisible({
      timeout: 5_000,
    });
  });

  // ── App-shell regression guards ──────────────────────────────
  // These tests lock in the mobile polish pass: no page-level h-scroll,
  // inputs don't trigger iOS zoom, rich content (thinking / python /
  // email preview) stays inside its container at a 375–412px width.

  test("viewport meta is configured for app-like mobile layout", async ({ page, login }) => {
    await login();
    const content = await page.locator('meta[name="viewport"]').getAttribute("content");
    expect(content).toBeTruthy();
    expect(content).toMatch(/initial-scale=1/);
    expect(content).toMatch(/viewport-fit=cover/);
    // Intentionally do NOT lock maximum-scale: pinch-zoom stays available
    // for accessibility. The 16px min input font (touch-device rule in
    // globals.css) is what suppresses iOS focus-zoom.
    expect(content).not.toMatch(/maximum-scale=1/);
  });

  test("composer input renders at >=16px so iOS doesn't zoom on focus", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    const fontSizePx = await composer.evaluate(
      (el) => parseFloat(getComputedStyle(el).fontSize),
    );
    expect(fontSizePx).toBeGreaterThanOrEqual(16);
  });

  test("page has no horizontal scroll at mobile width", async ({ page, login }) => {
    await login();
    const overflow = await page.evaluate(() => {
      const d = document.documentElement;
      return { scroll: d.scrollWidth, client: d.clientWidth };
    });
    // ±1 for subpixel rounding.
    expect(overflow.scroll).toBeLessThanOrEqual(overflow.client + 1);
  });

  test("thinking + python output blocks stay inside the viewport", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("render rich content");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*render rich content/i).first()).toBeVisible({
      timeout: 15_000,
    });

    // Details (thinking, stats, tool calls) hidden by default; flip the
    // global header toggle to reveal them. The reasoning block then
    // mounts already-expanded so the thinking text is visible without a
    // second tap.
    await page.getByRole("button", { name: /show details/i }).tap();
    await expect(page.getByText(/Let me think about this for a moment/i)).toBeVisible();

    // Python output now starts collapsed regardless of line count
    // (long one-liners were bleeding past the chat column). Tap to
    // reveal the stdout block and confirm it fits the viewport.
    const pythonToggle = page.getByRole("button", { name: /python output/i });
    await expect(pythonToggle).toBeVisible();
    await pythonToggle.tap();
    await expect(page.getByText(/mock result/).first()).toBeVisible();

    const overflow = await page.evaluate(() => {
      const d = document.documentElement;
      return { scroll: d.scrollWidth, client: d.clientWidth };
    });
    expect(overflow.scroll).toBeLessThanOrEqual(overflow.client + 1);
  });

  test("email approval card + body preview fit the mobile viewport", async ({ page, login }) => {
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("please send email to the team");
    await composer.press("Enter");

    // Approval card surfaces from the mock turn's tool.approval_required.
    const card = page
      .locator("div", { has: page.getByText(/Send this email\?/i) })
      .filter({ has: page.getByRole("button", { name: /^Send$/ }) })
      .first();
    await expect(card).toBeVisible({ timeout: 15_000 });

    // Card itself fits within the viewport horizontally.
    const cardBox = await card.boundingBox();
    const vw = page.viewportSize();
    expect(cardBox).not.toBeNull();
    expect(vw).not.toBeNull();
    if (cardBox && vw) {
      expect(cardBox.x).toBeGreaterThanOrEqual(0);
      expect(cardBox.x + cardBox.width).toBeLessThanOrEqual(vw.width + 1);
    }

    // Send cards now auto-expand by default — the body preview is
    // already on screen at render. Verify the toggle button exists with
    // its new "Hide email" label (so collapsing remains accessible),
    // then tap to collapse and re-expand to make sure neither state
    // breaks horizontal layout on mobile.
    const toggleBtn = page.getByRole("button", { name: /Hide email|Show email/ });
    await expect(toggleBtn).toBeVisible();
    await toggleBtn.tap(); // collapse
    await toggleBtn.tap(); // re-expand

    // After the round-trip, the page still must not scroll horizontally.
    const overflow = await page.evaluate(() => {
      const d = document.documentElement;
      return { scroll: d.scrollWidth, client: d.clientWidth };
    });
    expect(overflow.scroll).toBeLessThanOrEqual(overflow.client + 1);
  });

  test("message-action buttons have a comfortable touch target", async ({ page, login }) => {
    // Copy + Regenerate appear under the assistant reply. Their raw text
    // sits at ~11px with no padding on desktop — the .touch-target utility
    // expands them to ≥40px min-height on touch devices. We assert both
    // the rendered height and the tap-hit box stay ergonomic.
    await login();
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("tap target audit");
    await composer.press("Enter");
    await expect(page.getByText(/Mock reply to:\s*tap target audit/i).first()).toBeVisible({
      timeout: 15_000,
    });

    for (const name of [/^Copy$/, /^Regenerate$/]) {
      const btn = page.getByRole("button", { name });
      await expect(btn).toBeVisible();
      const box = await btn.boundingBox();
      expect(box).not.toBeNull();
      if (box) {
        // 40px (2.5rem) matches the .touch-target min-height. Below that
        // we're back to a ~14px hit area that misses finger taps.
        expect(box.height).toBeGreaterThanOrEqual(40);
      }
    }
  });

  test("reloading a conversation with rich history has no right-edge bleed at 375px", async ({ page, login }) => {
    // Regression guard for the "all mobile elements slightly bleed off the
    // right edge when a chat with history loads" bug. Root cause was
    // `<main className="grid">` with no explicit grid-template-columns: the
    // implicit auto column let main's internal grid track size to the
    // children's max-content (sibling sections + composer at ~413px) while
    // main itself stayed at the viewport width. `overflow: hidden` on main
    // made main a scroll container, so the track was allowed to grow past
    // main's visible width. Header, composer, and every message then laid
    // out at 413px on a 375px phone — the entire right 38px was clipped.
    // The empty-chat h-scroll assertion above didn't catch this because
    // the composer at idle can still fit in the track.
    await login();
    // iPhone SE width — narrower than the default Pixel 7 (412) mobile
    // viewport. 375px is where Safari users most often hit the bleed.
    await page.setViewportSize({ width: 375, height: 667 });
    const composer = page.getByPlaceholder(/message elcano ai/i);
    await composer.fill("render rich content please send email to the team");
    await composer.press("Enter");
    await expect(
      page.locator("div", { has: page.getByText(/Send this email\?/i) }).first()
    ).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(/Mock reply to:/i).first()).toBeVisible({ timeout: 20_000 });
    await page.reload();
    await expect(page.getByText(/Mock reply to:/i).first()).toBeVisible({ timeout: 20_000 });
    await page.waitForFunction(() => {
      const conversation = document.querySelector<HTMLElement>('[role="region"][aria-label="Conversation"]');
      if (!conversation) return false;
      const distanceFromBottom = conversation.scrollHeight - conversation.scrollTop - conversation.clientHeight;
      return distanceFromBottom <= 2;
    });
    const vw = page.viewportSize()!;
    const offenders = await page.evaluate((viewportWidth) => {
      const bad: Array<{ tag: string; cls: string; right: number }> = [];
      document.querySelectorAll<HTMLElement>("body *").forEach((el) => {
        const r = el.getBoundingClientRect();
        if (r.right > viewportWidth + 1) {
          bad.push({
            tag: el.tagName,
            cls: (el.className?.toString() || "").slice(0, 120),
            right: Math.round(r.right),
          });
        }
      });
      return bad;
    }, vw.width);
    expect(offenders, `elements extending past viewport ${vw.width}px`).toEqual([]);
    const doc = await page.evaluate(() => ({
      scroll: document.documentElement.scrollWidth,
      client: document.documentElement.clientWidth,
    }));
    expect(doc.scroll).toBeLessThanOrEqual(doc.client + 1);
  });

  test("pathologically wide assistant content scrolls inside its block, not the page", async ({ page, login }) => {
    // Regression guard for future rich-content additions: even if some
    // renderer produces a 4000px-wide element (long table, pre-wrap
    // code, etc.), the `html,body { max-width:100vw; overflow-x:hidden }`
    // in globals.css must keep the page itself from scrolling sideways.
    await login();
    await page.evaluate(() => {
      const wide = document.createElement("div");
      wide.id = "mobile-overflow-probe";
      wide.style.width = "4000px";
      wide.style.height = "1px";
      wide.textContent = "x".repeat(4000);
      document.body.appendChild(wide);
    });

    const overflow = await page.evaluate(() => {
      const d = document.documentElement;
      return { scroll: d.scrollWidth, client: d.clientWidth };
    });
    // If the fix regresses, scrollWidth jumps to ~4000 and this assertion
    // fires loudly.
    expect(overflow.scroll).toBeLessThanOrEqual(overflow.client + 1);
  });
});
