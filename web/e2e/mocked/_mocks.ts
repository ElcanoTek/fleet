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
  // Seed the conversation list (defaults to empty). Only the fields the sidebar
  // reads are required; the rest are filled with sensible defaults so specs can
  // pass a minimal `{ id, title }`.
  conversations?: Array<Record<string, unknown>>;
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
      },
    }),
  );
  await page.route("**/api/mcp-servers", (r: Route) => r.fulfill({ json: { servers: [] } }));
  const seededConversations = (opts.conversations ?? []).map((c, i) => ({
    persona: personaDefault,
    model: "test-model",
    pinned: false,
    archived_at: null,
    updated_at: 1_700_000_000 - i,
    created_at: 1_700_000_000 - i,
    labels: [],
    folder: null,
    ...c,
  }));
  await page.route("**/api/conversations", (r: Route) => {
    if (r.request().method() === "GET") return r.fulfill({ json: { conversations: seededConversations } });
    return r.fulfill({ json: {} });
  });
  // The same list read, with a query string. The glob above matches the bare
  // path only, so `?archived=true` and `?scope=team` fell through to the Next
  // proxy and 502'd against the absent Go backend — twice per page load for
  // the rail's team-shared counts alone. Registered AFTER the bare route
  // because Playwright checks the most recent registration first.
  //
  // `?scope=team` is the team-visible read (ADR-0057): every chat a teammate
  // shared, which the rail reduces into a per-project count for its
  // "{N} shared by your team" empty state. Serving the seeded team-visible
  // rows means the specs exercise that path instead of its failure branch.
  await page.route("**/api/conversations?*", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    const url = new URL(r.request().url());
    if (url.searchParams.get("scope") === "team") {
      return r.fulfill({
        json: {
          conversations: seededConversations.filter(
            (c) => (c as { team_visible?: boolean }).team_visible,
          ),
        },
      });
    }
    const archived = url.searchParams.get("archived") === "true";
    return r.fulfill({
      json: {
        conversations: seededConversations.filter((c) =>
          archived
            ? (c as { archived_at?: unknown }).archived_at !== null
            : (c as { archived_at?: unknown }).archived_at === null,
        ),
      },
    });
  });
  // Per-conversation GET (loadConversation): return the seeded summary with an
  // empty history so opening a conversation — on boot or via the keyboard —
  // resolves cleanly instead of 502-ing against the absent Go backend.
  // Per-conversation side fetches the shell fires on load. Without mocks
  // these fall through to the Next proxy and burn ~1s each before 502ing —
  // harmless locally, but under CI load the backlog makes menu/reveal
  // interactions flaky. Registered BEFORE the generic /conversations/* route
  // (later registrations win) — wait, Playwright checks the LAST registered
  // first, so register these AFTER it… they match more specifically anyway
  // because the generic pattern's single `*` cannot cross the extra segment.
  await page.route("**/api/conversations/*/inflight", (r: Route) =>
    r.fulfill({ json: { inflight: false } }),
  );
  await page.route("**/api/conversations/*/mcp-servers", (r: Route) =>
    r.fulfill({ json: { servers: [] } }),
  );
  await page.route("**/api/conversations/*", (r: Route) => {
    if (r.request().method() !== "GET") return r.fulfill({ json: {} });
    const url = new URL(r.request().url());
    const id = url.pathname.split("/").pop() ?? "";
    const found =
      seededConversations.find((c) => (c as { id?: string }).id === id) ??
      ({ id, title: "Conversation", persona: personaDefault, model: "test-model", pinned: false } as Record<
        string,
        unknown
      >);
    return r.fulfill({ json: { conversation: found, history: [] } });
  });
  await page.route("**/api/model-rankings", (r: Route) => r.fulfill({ json: { rankings: [] } }));
  await page.route("**/api/model-catalog", (r: Route) => r.fulfill({ json: { models: [] } }));
  await page.route("**/api/model-check**", (r: Route) => r.fulfill({ json: { ok: true } }));
  // The composer's /skill autocomplete roster. Left unmocked this 502s, and
  // the Next dev-tools error badge that raises sits bottom-left — exactly
  // over the collapsed rail's account button — swallowing test clicks.
  await page.route("**/api/skills", (r: Route) => r.fulfill({ json: { skills: [] } }));
  // Workspace-provider models + the catwalk catalog behind them: default to
  // "none configured" so pickers exercise their catalog/seed paths.
  await page.route("**/api/llm-provider-models", (r: Route) =>
    r.fulfill({ json: { models: [], providers: [] } }),
  );
  await page.route("**/api/catwalk-models", (r: Route) => r.fulfill({ json: { providers: [] } }));
  // client-config is fail-open in the UI; default to neutral branding/cards so
  // the shell never 500s on the upstream-unreachable real route. Specs that
  // assert config-driven cards override this route afterward.
  await page.route("**/api/client-config", (r: Route) =>
    r.fulfill({ json: { branding: {}, empty_state: { cards: [] } } }),
  );
}
