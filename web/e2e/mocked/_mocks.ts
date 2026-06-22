import type { Page, Route } from "@playwright/test";

// Shared route-mock helpers for the mocked suite. Each spec composes these to
// stub the exact /api/* surface its view touches, then layers spec-specific
// routes (the chat SSE stream, the orchestrator task list, …) on top. Playwright
// matches routes most-recently-registered-first, so a spec can override any of
// these by registering a narrower route AFTER calling the installer.

// ── Server-sent-events framing ─────────────────────────────────────────────
// Serializes SSE frames exactly as chat-server emits them: an `event:` line, an
// optional `id:` line, then a `data:` line carrying the JSON payload. Frames are
// separated by a blank line. Matches the wire format the chat shell's parser in
// src/app/lib/sse.ts consumes.
export function sse(frames: Array<{ event: string; data: unknown; id?: number }>): string {
  return (
    frames
      .map((f) => {
        const lines = [`event: ${f.event}`];
        if (f.id !== undefined) lines.push(`id: ${f.id}`);
        lines.push(`data: ${JSON.stringify(f.data)}`);
        return lines.join("\n");
      })
      .join("\n\n") + "\n\n"
  );
}

export function fulfillSse(route: Route, frames: Array<{ event: string; data: unknown; id?: number }>) {
  return route.fulfill({
    status: 200,
    headers: { "Content-Type": "text/event-stream; charset=utf-8", "Cache-Control": "no-cache" },
    body: sse(frames),
  });
}

// ── Chat shell boot ─────────────────────────────────────────────────────────
// The set of /api/* calls the chat experience makes on mount (session, version,
// personas, server-config, the MCP catalog, conversation history, model lists).
// `personaDefault` controls which persona the shell selects after load — the
// empty-state protocol-pill cards only render under the "victoria" persona, so
// the client-config spec passes "victoria" while plain chat specs leave the
// neutral "default".
export type ChatBootOptions = {
  personaDefault?: string;
  personas?: Array<{ id: string; name: string }>;
};

export async function mockChatBoot(page: Page, opts: ChatBootOptions = {}) {
  const personaDefault = opts.personaDefault ?? "default";
  const personas =
    opts.personas ??
    (personaDefault === "victoria"
      ? [{ id: "victoria", name: "Victoria" }]
      : [{ id: "default", name: "Default" }]);

  await page.route("**/api/session", (r: Route) => r.fulfill({ json: { email: "e2e@example.com" } }));
  await page.route("**/api/version", (r: Route) => r.fulfill({ json: { build_id: "test" } }));
  await page.route("**/api/personas", (r: Route) =>
    r.fulfill({ json: { personas, default: personaDefault } }),
  );
  await page.route("**/api/server-config", (r: Route) =>
    r.fulfill({
      json: {
        lockdown_available: false,
        lockdown_only: false,
        lockdown_allowed_models: [],
        julesEnabled: false,
      },
    }),
  );
  await page.route("**/api/mcp-servers", (r: Route) => r.fulfill({ json: { servers: [] } }));
  await page.route("**/api/conversations", (r: Route) => {
    if (r.request().method() === "GET") return r.fulfill({ json: { conversations: [] } });
    return r.fulfill({ json: {} });
  });
  await page.route("**/api/model-rankings", (r: Route) => r.fulfill({ json: { rankings: [] } }));
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: { models: [] } }));
  await page.route("**/api/model-check**", (r: Route) => r.fulfill({ json: { ok: true } }));
  // client-config is fail-open in the UI; default to neutral branding/cards so
  // the shell never 500s on the upstream-unreachable real route. Specs that
  // assert config-driven cards override this route afterward.
  await page.route("**/api/client-config", (r: Route) =>
    r.fulfill({ json: { branding: {}, empty_state: { cards: [] } } }),
  );
}
