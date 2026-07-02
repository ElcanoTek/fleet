#!/usr/bin/env node
// Record the web demo GIF source videos (#540) against a RUNNING local fleet
// stack — real backend, real sandbox, real model — so what the README shows is
// the true product. See docs/generating-demo-gif.md; driven by
// docs/scripts/generate-web-gifs.sh, which owns the webm→gif conversion.
//
// Env: DEMO_BASE_URL (default http://127.0.0.1:18300), DEMO_EMAIL,
// DEMO_PASSWORD, DEMO_OUT_DIR (default /tmp/fleet-web-demos).
//
// Two takes:
//   chat.webm — a launch-planning ask: tools run in the sandbox, the answer
//               streams in live, markdown renders.
//   ops.webm  — the Operations Center: a healthy fleet of recurring
//               automations, then the Upcoming view.

import { mkdirSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// The script lives in docs/scripts but playwright is installed under web/ —
// resolve it from there explicitly so this runs from any cwd.
const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const require = createRequire(join(repoRoot, "web", "package.json"));
const { chromium } = require("playwright");

const BASE = process.env.DEMO_BASE_URL || "http://127.0.0.1:18300";
const EMAIL = process.env.DEMO_EMAIL || "smoke@e2e.local";
const PASSWORD = process.env.DEMO_PASSWORD || "smoke-password-123";
const OUT = process.env.DEMO_OUT_DIR || "/tmp/fleet-web-demos";

const VIEWPORT = { width: 1280, height: 800 };
const PROMPT =
  "We just signed the Meridian deal 🎉 — plan the kickoff: build a 6-week onboarding timeline with owners, and compute the first-year revenue at $18.5k/month with the 12% multi-region uplift.";

async function login(page) {
  await page.goto(`${BASE}/login`);
  await page.getByLabel(/email/i).fill(EMAIL);
  await page.getByLabel(/password/i).fill(PASSWORD);
  await page.getByRole("button", { name: /sign in|log in/i }).click();
  await page.waitForURL((u) => !u.pathname.startsWith("/login"), { timeout: 30_000 });
}

async function typeSlowly(locator, text, delayMs = 28) {
  await locator.click();
  await locator.pressSequentially(text, { delay: delayMs });
}

async function recordChat(browser) {
  const ctx = await browser.newContext({
    viewport: VIEWPORT,
    recordVideo: { dir: OUT, size: VIEWPORT },
    colorScheme: "dark",
  });
  const page = await ctx.newPage();
  await login(page);
  await page.waitForTimeout(1500);

  const composer = page.locator("textarea").first();
  await typeSlowly(composer, PROMPT);
  await page.waitForTimeout(600);
  await composer.press("Enter");

  // Let the real turn play out: tool chips appear, then the streamed answer.
  // Cap the take; a good take shows 2+ tools and a rendered markdown answer.
  await page.waitForTimeout(90_000);
  await page.mouse.wheel(0, 800);
  await page.waitForTimeout(4_000);

  await ctx.close(); // flushes the video
  const video = await page.video().path();
  console.log("chat take:", video);
}

async function recordOps(browser) {
  const ctx = await browser.newContext({
    viewport: VIEWPORT,
    recordVideo: { dir: OUT, size: VIEWPORT },
    colorScheme: "dark",
  });
  const page = await ctx.newPage();
  await login(page);
  await page.goto(`${BASE}/orchestrator`);
  await page.waitForTimeout(2000);

  // The Operations Center has its own operator sign-in (sched users).
  const opsUser = process.env.DEMO_OPS_USER || "opsdemo";
  const opsPass = process.env.DEMO_OPS_PASSWORD || "ops-demo-password-1";
  try {
    await page.locator("#orch-username").fill(opsUser, { timeout: 4000 });
    await page.locator("#orch-password").fill(opsPass);
    await page.getByRole("button", { name: "Login with username and password" }).click();
    await page.waitForTimeout(3000);
  } catch {
    // already signed in — fine
  }
  await page.waitForTimeout(2500);

  // Browse the fleet of automations, then the forward-looking Upcoming view.
  await page.mouse.wheel(0, 400);
  await page.waitForTimeout(2500);
  const upcoming = page.getByRole("button", { name: /upcoming/i }).or(page.getByText(/^Upcoming$/i).first());
  try {
    await upcoming.first().click({ timeout: 5000 });
    await page.waitForTimeout(4000);
  } catch {
    console.log("ops take: Upcoming tab not found — kept the dashboard view");
  }
  await page.waitForTimeout(2000);

  await ctx.close();
  const video = await page.video().path();
  console.log("ops take:", video);
}

mkdirSync(OUT, { recursive: true });
const only = process.env.DEMO_ONLY || "";
const browser = await chromium.launch();
if (only !== "ops") await recordChat(browser);
if (only !== "chat") await recordOps(browser);
await browser.close();
console.log("done — videos in", OUT);
