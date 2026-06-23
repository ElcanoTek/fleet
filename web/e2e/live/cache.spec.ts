import { test, expect } from "./fixtures";

// LIVE cache-busting contract against the REAL Next server (the mocked lane
// can't cover this — it route-intercepts /api/version). What matters for "no
// stale clients after a deploy":
//   1. /api/version returns the build id in both body and the X-App-Version
//      header (the client polls it on visibilitychange and reloads on drift).
//   2. /api/version is uncacheable (a cached copy would defeat drift detection).
//   3. HTML forces revalidation (no-store or no-cache) so a deploy can't leave a
//      stale shell pointing at deleted /_next/static chunks.
//   4. The build id is stable within a session (a consistent baseline to diff).

test.describe("live cache-busting contract (real Next server)", () => {
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
    await login();
    const res = await page.request.get("/api/version");
    const cc = (res.headers()["cache-control"] ?? "").toLowerCase();
    expect(cc).toContain("no-store");
  });

  test("HTML responses force revalidation", async ({ page, login }) => {
    await login();
    const res = await page.request.get("/");
    const cc = (res.headers()["cache-control"] ?? "").toLowerCase();
    expect(
      cc.includes("no-store") || cc.includes("no-cache"),
      `/ Cache-Control must force revalidation, got: ${cc}`,
    ).toBe(true);
  });

  test("build id is stable within a session", async ({ page, login }) => {
    await login();
    const a = (await (await page.request.get("/api/version")).json()) as { buildId?: string };
    const b = (await (await page.request.get("/api/version")).json()) as { buildId?: string };
    expect(a.buildId).toBe(b.buildId);
  });
});
