import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
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

// A community-provenance entry: adding it is gated on the consent dialog,
// which is where this page's modal chrome (backdrop, panel, Escape) lives.
const ACME = {
  name: "acme_notes",
  display_name: "Acme Notes",
  description: "Community-run notes server.",
  url: "https://mcp.acme.example/mcp",
  vendor: "Acme Collective",
  category: "productivity",
  provenance: "community",
  trust: "third_party",
};

// One optional bundled connector, on by operator default: its availability
// row is the switch + clickable state-text pair.
const BUNDLED_IMG = {
  name: "image_generation",
  display_name: "Image generation",
  description: "Bundled image tools.",
  tool_count: 2,
  optional: true,
  enabled_by_default: true,
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
  catalog: unknown = CATALOG,
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
      return { ok: true, status: 200, json: async () => catalog };
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

// Accessibility contracts for the page's overlay chrome and its switch rows.
// The dialogs used to hang `role="dialog"` on the full-screen backdrop and
// close it from an onClick on that <div>: a mouse-only dismissal, with focus
// left behind on the page. These lock in the keyboard paths that replaced it.
describe("ConnectionsPage consent dialog", () => {
  const catalogWithConsent = { ...CATALOG, third_party: [BROWSERBASE, ACME] };

  it("takes focus as a labelled dialog and closes on Escape without adding", async () => {
    let posted = false;
    vi.stubGlobal(
      "fetch",
      mockFetch(() => {
        posted = true;
        return { status: 200, body: { id: "srv1" } };
      }, catalogWithConsent),
    );
    visit("");

    // Activate the way a keyboard user does — focus on the trigger, then
    // fire it — so the focus hand-back below is exercised. (fireEvent.click
    // alone does not move focus in jsdom the way a real pointer click does.)
    const addButton = await screen.findByTestId("dir-add-acme_notes");
    addButton.focus();
    fireEvent.click(addButton);
    const dialog = await screen.findByRole("dialog", {
      name: "Connect Acme Notes?",
    });
    // The panel — not the backdrop — is the dialog, and focus moves into it.
    expect(document.activeElement).toBe(dialog);

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posted).toBe(false);
    // Dismissing hands focus back to the control that opened the dialog.
    expect(document.activeElement).toBe(addButton);
  });

  it("exposes the click-outside dismissal as a named control", async () => {
    vi.stubGlobal("fetch", mockFetch(undefined, catalogWithConsent));
    visit("");

    fireEvent.click(await screen.findByTestId("dir-add-acme_notes"));
    await screen.findByRole("dialog", { name: "Connect Acme Notes?" });
    fireEvent.click(
      screen.getByRole("button", {
        name: "Dismiss the Acme Notes connect dialog",
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("ConnectionsPage availability toggle", () => {
  it("announces one control (the switch) but keeps the state text clickable", async () => {
    const calls: { url: string; init?: RequestInit }[] = [];
    const base = mockFetch(undefined, { ...CATALOG, bundled: [BUNDLED_IMG] });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url, init });
        return base(url, init);
      }),
    );
    visit("");

    const card = await screen.findByTestId("bundled-card-image_generation");
    expect(
      within(card).getByRole("switch", { name: "Enable Image generation for me" }),
    ).toBeInTheDocument();
    // The state text duplicates that switch, so it is deliberately absent
    // from the accessibility tree instead of being a second, unexplained
    // control next to it…
    expect(
      within(card).queryByRole("button", { name: "Enabled for me" }),
    ).toBeNull();
    // …while remaining the pointer hit target it always was.
    fireEvent.click(within(card).getByText("Enabled for me"));
    await waitFor(() =>
      expect(
        calls.some(
          (c) => c.url === "/api/connector-prefs" && c.init?.method === "PUT",
        ),
      ).toBe(true),
    );
    const put = calls.find(
      (c) => c.url === "/api/connector-prefs" && c.init?.method === "PUT",
    );
    expect(JSON.parse(String(put?.init?.body))).toMatchObject({
      kind: "bundled",
      connector_id: "image_generation",
      enabled: false,
    });
  });
});
