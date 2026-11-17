import { describe, expect, it } from "vitest";
import {
  bootstrapFailureDestination,
  classifyBootstrapFailure,
  withReturnTo,
} from "./bootstrapFailure";

// Regression contract for the /chat ↔ /login reload loop: a backend that is
// down answers the proxied bootstrap fetches with 502/503/504 while the
// session cookie is still valid. Treating those as "unauthenticated" redirects
// to /login, which the middleware bounces straight back to /chat — looping.
// Only a real auth verdict may redirect.
describe("classifyBootstrapFailure", () => {
  it("treats 401 as unauthenticated (redirect to /login) and 403 as forbidden (redirect to /no-access)", () => {
    expect(classifyBootstrapFailure(401)).toBe("unauthenticated");
    expect(classifyBootstrapFailure(403)).toBe("forbidden");
    expect(bootstrapFailureDestination(401)).toBe("/login");
    expect(bootstrapFailureDestination(403)).toBe("/no-access");
    for (const status of [500, 502, 503, 504, 429]) {
      expect(bootstrapFailureDestination(status)).toBeNull();
    }
  });

  it("sends sign-in on to the page's own link when one is given", () => {
    const link = "/chat?persona=user%3Atwin&draft=Book%20intake%20ELC07431";
    expect(bootstrapFailureDestination(401, link)).toBe(
      `/login?return_to=${encodeURIComponent(link)}`,
    );
    expect(bootstrapFailureDestination(401, null)).toBe("/login");
    expect(bootstrapFailureDestination(401, "")).toBe("/login");
    // Only sign-in carries it: no-access and an unreachable backend do not.
    expect(bootstrapFailureDestination(403, link)).toBe("/no-access");
    expect(bootstrapFailureDestination(502, link)).toBeNull();
  });

  it("adds return_to to a sign-in entry only when there is a link", () => {
    expect(withReturnTo("/login", "/orchestrator?task=t1")).toBe(
      "/login?return_to=%2Forchestrator%3Ftask%3Dt1",
    );
    expect(withReturnTo("/login", null)).toBe("/login");
    expect(withReturnTo("/login")).toBe("/login");
  });

  it("treats backend-down statuses as unreachable, never a redirect", () => {
    expect(classifyBootstrapFailure(502)).toBe("unreachable");
    expect(classifyBootstrapFailure(503)).toBe("unreachable");
    expect(classifyBootstrapFailure(504)).toBe("unreachable");
  });

  it("treats other server errors as unreachable rather than looping", () => {
    expect(classifyBootstrapFailure(500)).toBe("unreachable");
    expect(classifyBootstrapFailure(429)).toBe("unreachable");
  });
});
