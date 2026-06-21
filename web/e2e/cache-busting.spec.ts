import { test, expect } from "./fixtures";

// Cache-busting contract (what actually matters for "no stale clients
// after a deploy"):
//
//   1. /api/version returns the current build id in body + header.
//      Client polls this on visibilitychange and reloads on drift.
//   2. Build id is stable within a session — drift detection only
//      makes sense against a consistent baseline.
//   3. HTML is not cached long enough for browsers to serve a stale
//      shell that references deleted /_next/static chunks. Either
//      Cache-Control: no-store (from our middleware) or no-cache
//      (from Next's default RSC headers) satisfies that — both force
//      revalidation before the cached copy is used.
//
// The client-side silent reload itself isn't asserted here because
// we'd need to fake a second build at runtime; what we CAN assert
// is that the probe surface is present. If the probe endpoint or
// the HTML cache policy regresses, cache busting breaks.

test.describe("cache busting", () => {
  test("/api/version returns a build id in both header and body", async ({ page, login }) => {
    await login();
    const res = await page.request.get("/api/version");
    expect(res.status()).toBe(200);
    const headerId = res.headers()["x-app-version"];
    const body = (await res.json()) as { buildId?: string };
    expect(headerId, "X-App-Version header").toBeTruthy();
    expect(body.buildId, "body.buildId").toBeTruthy();
    expect(body.buildId).toBe(headerId);
  });

  test("/api/version is not cacheable", async ({ page, login }) => {
    // The client polls this on visibilitychange, so a cached copy
    // would defeat the whole mechanism.
    await login();
    const res = await page.request.get("/api/version");
    const cc = (res.headers()["cache-control"] ?? "").toLowerCase();
    expect(cc).toContain("no-store");
  });

  test("HTML responses force revalidation", async ({ page, login }) => {
    // Either no-store (from our middleware) or no-cache (from Next's
    // default RSC headers) satisfies the contract: browser must talk
    // to the server before reusing a cached copy, so a deploy can't
    // leave a stale HTML shell pointing at deleted chunks.
    await login();
    const res = await page.request.get("/");
    const cc = (res.headers()["cache-control"] ?? "").toLowerCase();
    expect(
      cc.includes("no-store") || cc.includes("no-cache"),
      `/ Cache-Control must force revalidation, got: ${cc}`,
    ).toBe(true);
  });

  test("build id is stable within a session", async ({ page, login }) => {
    // Two probes in a row must agree — if they don't, something's
    // wrong (two workers out of sync, stale config, missing pin).
    await login();
    const a = await (await page.request.get("/api/version")).json();
    const b = await (await page.request.get("/api/version")).json();
    expect(a.buildId).toBe(b.buildId);
  });
});
