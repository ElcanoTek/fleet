import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import ConnectionsPage from "./page";

// Settings → Connections — the guided directory add plus the ?connector= deep
// link. Load-bearing assertions: a deep-linked entry lands filtered with its
// key form already open, the add POST carries the manifest's api_key_query
// (Browserbase's server takes the key as a query parameter, not a header — an
// add without it fails its validation probe), and a rejected key keeps the
// form and the typed value so it can be corrected in place.

vi.mock("../useIsAdmin", () => ({
  useIsAdmin: () => "member",
}));

const BROWSERBASE = {
  name: "browserbase",
  display_name: "Browserbase",
  description: "Cloud browser automation via Stagehand.",
  url: "https://mcp.browserbase.com/mcp?keepAlive=true",
  vendor: "Browserbase, Inc.",
  category: "web-search",
  provenance: "official",
  auth: "api_key",
  api_key_query: "browserbaseApiKey",
  setup_hint: "In the Browserbase dashboard, copy your API key.",
  setup_url: "https://www.browserbase.com/overview",
  featured: true,
  trust: "third_party",
};

const CATALOG = {
  bundled: [],
  third_party: [BROWSERBASE],
  remote_mcp_enabled: true,
};

// Stubs every endpoint the page touches on mount; `onAdd` decides what the
// remote-server POST returns.
function mockFetch(
  onAdd?: (body: Record<string, unknown>) => { status: number; body: unknown },
) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    if (url.startsWith("/api/remote-mcp-servers") && method === "POST") {
      const parsed = JSON.parse(String(init?.body ?? "{}")) as Record<
        string,
        unknown
      >;
      const out = onAdd?.(parsed) ?? { status: 200, body: { id: "srv1" } };
      return {
        ok: out.status < 400,
        status: out.status,
        json: async () => out.body,
        text: async () => JSON.stringify(out.body),
      };
    }
    if (url.startsWith("/api/remote-mcp-servers")) {
      return {
        ok: true,
        status: 200,
        json: async () => ({ servers: [], shares: {}, shared_with_me: [] }),
      };
    }
    if (url.startsWith("/api/mcp-catalog")) {
      return { ok: true, status: 200, json: async () => CATALOG };
    }
    if (url.startsWith("/api/connector-prefs")) {
      return { ok: true, status: 200, json: async () => ({ prefs: [] }) };
    }
    // /api/orchestrator/mcp-servers (credential-accounts panel) and anything
    // else incidental: empty success.
    return { ok: true, status: 200, json: async () => ({ servers: [] }) };
  });
}

function visit(search: string) {
  window.history.replaceState({}, "", `/settings/connections${search}`);
  return render(<ConnectionsPage />);
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.replaceState({}, "", "/settings/connections");
});

// jsdom has no scrollIntoView; the deep-link effect calls it on the card.
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

describe("ConnectionsPage ?connector= deep link", () => {
  it("filters the directory to the entry and opens its key form pre-focused", async () => {
    vi.stubGlobal("fetch", mockFetch());
    visit("?connector=browserbase");

    const form = await screen.findByTestId("dir-form-browserbase");
    expect(form).toBeTruthy();
    // The search box was seeded so the directory shows the linked entry.
    const search = screen.getByLabelText(
      "Search connector directory",
    ) as HTMLInputElement;
    expect(search.value).toBe("browserbase");
    // One paste away: the key field already holds focus.
    const key = screen.getByPlaceholderText(
      "paste your key (stored encrypted, never shown again)",
    ) as HTMLInputElement;
    expect(document.activeElement).toBe(key);
    // The one-shot param is stripped, like the OAuth callback params.
    expect(window.location.search).toBe("");
  });

  it("does not open a form when the entry is unknown", async () => {
    vi.stubGlobal("fetch", mockFetch());
    visit("?connector=nonexistent");
    await screen.findByTestId("dir-filter-bar");
    expect(screen.queryByTestId("dir-form-browserbase")).toBeNull();
  });
});

describe("ConnectionsPage guided api_key add", () => {
  it("sends the manifest's api_key_query with the key", async () => {
    let posted: Record<string, unknown> | null = null;
    vi.stubGlobal(
      "fetch",
      mockFetch((body) => {
        posted = body;
        return { status: 200, body: { id: "srv1", tool_count: 6 } };
      }),
    );
    visit("?connector=browserbase");

    await screen.findByTestId("dir-form-browserbase");
    fireEvent.change(
      screen.getByPlaceholderText(
        "paste your key (stored encrypted, never shown again)",
      ),
      { target: { value: "bb_test_key" } },
    );
    fireEvent.click(screen.getByTestId("dir-form-add-browserbase"));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({
      name: "browserbase",
      url: "https://mcp.browserbase.com/mcp?keepAlive=true",
      auth: "api_key",
      api_key: "bb_test_key",
      api_key_query: "browserbaseApiKey",
    });
  });

  it("keeps the form and the typed key when validation rejects it", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch(() => ({
        status: 400,
        body: "the server did not accept this API key",
      })),
    );
    visit("?connector=browserbase");

    await screen.findByTestId("dir-form-browserbase");
    const key = screen.getByPlaceholderText(
      "paste your key (stored encrypted, never shown again)",
    ) as HTMLInputElement;
    fireEvent.change(key, { target: { value: "bb_bad_key" } });
    fireEvent.click(screen.getByTestId("dir-form-add-browserbase"));

    // The rejected add must leave the form open with the value intact so the
    // user can fix the key in place.
    await waitFor(() => {
      expect(screen.getByTestId("dir-form-browserbase")).toBeTruthy();
      expect(key.value).toBe("bb_bad_key");
    });
  });
});
