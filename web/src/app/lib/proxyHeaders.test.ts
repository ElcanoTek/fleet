import { describe, expect, it } from "vitest";

import { forwardedHeaders } from "./proxyHeaders";

/**
 * Regression coverage for #896. Both proxy funnels re-emitted only
 * `Content-Type`, so `Content-Disposition` — which four Go export handlers set
 * on purpose — never reached the browser, and "Export CSV" downloaded a file
 * named `export` with no extension.
 *
 * These are the assertions that would have caught it.
 */
describe("forwardedHeaders", () => {
  function upstream(headers: Record<string, string>): Response {
    return new Response("body", { headers });
  }

  it("forwards Content-Disposition — the #896 regression", () => {
    const h = forwardedHeaders(
      upstream({
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": 'attachment; filename="Omnicom-Pacing.csv"',
      }),
    );
    expect(h.get("Content-Disposition")).toBe('attachment; filename="Omnicom-Pacing.csv"');
    expect(h.get("Content-Type")).toBe("text/csv; charset=utf-8");
  });

  it("forwards the caching validators", () => {
    const h = forwardedHeaders(
      upstream({
        "Cache-Control": "public, max-age=300",
        "Last-Modified": "Wed, 29 Jul 2026 12:00:00 GMT",
        ETag: '"abc123"',
      }),
    );
    expect(h.get("Cache-Control")).toBe("public, max-age=300");
    expect(h.get("Last-Modified")).toBe("Wed, 29 Jul 2026 12:00:00 GMT");
    expect(h.get("ETag")).toBe('"abc123"');
  });

  // A bodyless Response is the only way upstream genuinely sends no
  // Content-Type: `new Response("x")` auto-sets text/plain;charset=UTF-8, so a
  // string-bodied fixture would be testing forwarding, not the fallback.
  it("defaults Content-Type to JSON when upstream omits it", () => {
    // Preserves both funnels' previous behavior for API responses.
    const h = forwardedHeaders(new Response(null, { status: 204 }));
    expect(h.get("Content-Type")).toBe("application/json");
  });

  it("honors a caller-supplied fallback Content-Type", () => {
    const h = forwardedHeaders(new Response(null, { status: 204 }), "text/plain");
    expect(h.get("Content-Type")).toBe("text/plain");
  });

  it("prefers upstream's Content-Type over the fallback", () => {
    const h = forwardedHeaders(upstream({ "Content-Type": "text/csv" }), "application/json");
    expect(h.get("Content-Type")).toBe("text/csv");
  });

  it("does NOT forward Content-Length", () => {
    // fetch() transparently decodes a compressed upstream body, so the upstream
    // length can describe a different byte count than the one re-emitted — a
    // mismatch truncates the response. The runtime computes it correctly.
    const h = forwardedHeaders(upstream({ "Content-Length": "4096" }));
    expect(h.get("Content-Length")).toBeNull();
  });

  it("does not forward headers outside the allowlist", () => {
    // Upstream auth/identity plumbing must not leak to the browser.
    const h = forwardedHeaders(
      upstream({
        "X-User-Email": "alice@example.com",
        "Set-Cookie": "session=secret",
        Server: "fleet-internal/1.0",
      }),
    );
    expect(h.get("X-User-Email")).toBeNull();
    expect(h.get("Set-Cookie")).toBeNull();
    expect(h.get("Server")).toBeNull();
  });

  it("omits a header upstream did not send rather than emitting empty", () => {
    const h = forwardedHeaders(upstream({ "Content-Type": "application/json" }));
    expect(h.has("Content-Disposition")).toBe(false);
    expect(h.has("ETag")).toBe(false);
  });
});
