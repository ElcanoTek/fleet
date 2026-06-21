import { test, expect } from "./fixtures";

// Confirms the CSRF defense is wired on the mutating API routes. A
// forged Origin (pretending to be a different site's form submitting to
// ours) must be rejected with 403, even when the user has a valid
// session cookie.

test.describe("CSRF", () => {
  test("mutating API routes reject cross-origin POST with 403", async ({ page, login }) => {
    await login();

    // Try to POST to /api/chat with an Origin from another domain, using
    // the real browser's session cookie. Must 403.
    const resp = await page.request.post("/api/chat", {
      headers: { Origin: "https://evil.example.com" },
      data: JSON.stringify({ message: "csrf probe", persona: "generic" }),
    });
    expect(resp.status()).toBe(403);
  });

  test("missing Origin on a mutating route is rejected", async ({ page, login }) => {
    await login();

    // APIRequestContext doesn't set Origin by default, so this is the
    // real-world attacker-via-curl case. Must 403.
    const resp = await page.request.post(
      "/api/conversations",
      { data: JSON.stringify({ title: "no-origin" }) },
    );
    expect(resp.status()).toBe(403);
  });

  test("matching Origin is accepted (sanity)", async ({ page, login }) => {
    await login();
    const origin = new URL(page.url()).origin;

    const resp = await page.request.post("/api/conversations", {
      headers: { Origin: origin, "Content-Type": "application/json" },
      data: JSON.stringify({ title: "sanity", persona: "generic" }),
    });
    // Any 2xx means the CSRF gate let it through; the handler decides
    // what to do downstream.
    expect(resp.status()).toBeLessThan(300);
  });
});
