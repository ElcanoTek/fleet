import { test, expect } from "./fixtures";

// LIVE same-origin (CSRF) enforcement against the REAL Next middleware + API
// routes. A forged or absent Origin on a mutating route must be rejected even
// with a valid session cookie; a matching Origin must pass the gate. This proves
// the actual middleware behaviour end-to-end — the thing under test is the gate
// itself, so it runs against the booted stack rather than a route mock.
//
// The assertions key on the gate's own verdict (403 vs not-403), not on
// downstream handler semantics, so the test stays robust to body/validation
// changes in the handlers themselves.

test.describe("live CSRF / same-origin enforcement", () => {
  test("a cross-origin POST to a mutating route is rejected with 403", async ({ page, login }) => {
    await login();
    const resp = await page.request.post("/api/chat", {
      headers: { Origin: "https://evil.example.com", "Content-Type": "application/json" },
      data: JSON.stringify({ message: "csrf probe", persona: "default" }),
    });
    expect(resp.status()).toBe(403);
  });

  test("a POST with no Origin header is rejected with 403", async ({ page, login }) => {
    await login();
    // APIRequestContext does not set Origin by default — the attacker-via-curl case.
    const resp = await page.request.post("/api/conversations", {
      headers: { "Content-Type": "application/json" },
      data: JSON.stringify({ title: "no-origin" }),
    });
    expect(resp.status()).toBe(403);
  });

  test("a same-origin POST passes the CSRF gate", async ({ page, login }) => {
    await login();
    const origin = new URL(page.url()).origin;
    const resp = await page.request.post("/api/conversations", {
      headers: { Origin: origin, "Content-Type": "application/json" },
      data: JSON.stringify({ title: "same-origin", persona: "default" }),
    });
    // The gate let it through; whatever the handler decides downstream, it must
    // not be the CSRF rejection.
    expect(resp.status()).not.toBe(403);
  });
});
