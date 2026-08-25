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

// Multi-login seats (#988): two logins under one connection name, the
// unlabeled one default; plus an api_key connection matching the Browserbase
// directory entry.
const GAMMA_PRIMARY = {
  id: "g1",
  name: "gamma",
  url: "https://mcp.gamma.app/mcp",
  transport: "http",
  status: "connected",
  auth_kind: "oauth",
  account: "",
  is_default: true,
  created_at: 1,
  updated_at: 1,
};
const GAMMA_WORK = { ...GAMMA_PRIMARY, id: "g2", account: "work", is_default: false };
const BB_PRIMARY = {
  ...GAMMA_PRIMARY,
  id: "b1",
  name: "browserbase",
  url: BROWSERBASE.url,
  auth_kind: "api_key",
};

const EMPTY_LIST = { servers: [], shares: {}, shared_with_me: [] };

// Stubs every endpoint the page touches on mount; `onAdd` decides what the
// remote-server POST returns; `list` is what GET /api/remote-mcp-servers
// returns (own seats, shares, shared-with-me).
function mockFetch(
  onAdd?: (body: Record<string, unknown>) => { status: number; body: unknown },
  catalog: unknown = CATALOG,
  list: unknown = EMPTY_LIST,
) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    // Per-seat actions (set default / rename / sign out / rotate key): 204.
    if (/^\/api\/remote-mcp-servers\/[^/]+\/(default|account|signout|key)$/.test(url)) {
      return { ok: true, status: 204, json: async () => null, text: async () => "" };
    }
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
      return { ok: true, status: 200, json: async () => list };
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
    //
    // Asserted through waitFor, not inline. The focus is applied by a passive
    // effect (the card's apiKeyRef focus, page.tsx), and the state update that
    // opens the form comes from the catalog fetch resolving OUTSIDE act — so
    // React schedules that effect on a macrotask. findByTestId above resolves
    // the moment the form NODE appears, which is a commit earlier. There is a
    // genuine window in which the form is in the DOM and focus has not landed
    // yet; on an unloaded machine the flush wins that race every time, and on a
    // loaded CI runner it does not. Sampling document.activeElement at one
    // arbitrary instant is what made this test flaky (it failed once on a
    // docs-only PR, reporting activeElement as <body>). Retrying the assertion
    // tests the same guarantee — focus ends up in the key field — without
    // depending on which tick it lands in.
    const key = screen.getByPlaceholderText(
      "paste your key (stored encrypted, never shown again)",
    ) as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(key));
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
    // waitFor for the same reason as the deep-link test above: findByRole
    // resolves when the dialog node appears, and the panel's focus() is a
    // passive effect that may not have run in that tick.
    await waitFor(() => expect(document.activeElement).toBe(dialog));

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posted).toBe(false);
    // Dismissing hands focus back to the control that opened the dialog. The
    // hand-back runs on unmount, one tick after the dialog leaves the DOM that
    // the waitFor above is watching for — so this one waits too.
    await waitFor(() => expect(document.activeElement).toBe(addButton));
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

// Multiple logins per connection (#988). One visual group per name, one row
// per seat with its account badge; the default seat is marked; "Set default"
// only appears on non-default seats of a multi-seat group; adding a second
// login REQUIRES a label (the backend rejects an unlabeled second seat) and
// the add carries the label plus, for api_key servers, the manifest's key
// transport.
describe("ConnectionsPage multi-login seats", () => {
  // Records every fetch call while delegating to mockFetch.
  function recording(
    onAdd?: (body: Record<string, unknown>) => { status: number; body: unknown },
    list?: unknown,
  ) {
    const calls: { url: string; init?: RequestInit }[] = [];
    const base = mockFetch(onAdd, CATALOG, list);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        calls.push({ url, init });
        return base(url, init);
      }),
    );
    return calls;
  }

  it("groups seats under one name, badges each login, and marks the default", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch(undefined, CATALOG, { ...EMPTY_LIST, servers: [GAMMA_PRIMARY, GAMMA_WORK] }),
    );
    visit("");

    const group = await screen.findByTestId("remote-group-gamma");
    expect(within(group).getByText("primary")).toBeInTheDocument();
    expect(within(group).getByText("work")).toBeInTheDocument();
    // Exactly one seat is the default, and only the OTHER one offers to
    // become it.
    expect(within(group).getAllByText("Default")).toHaveLength(1);
    expect(within(group).getAllByRole("button", { name: "Set default" })).toHaveLength(1);
    // Every seat can be renamed.
    expect(within(group).getAllByRole("button", { name: "Rename" })).toHaveLength(2);
    // The seat semantics are explained once, under the multi-seat group.
    expect(within(group).getByText(/Chats use the default login/)).toBeInTheDocument();
  });

  it("does not offer Set default on a single-seat group", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch(undefined, CATALOG, { ...EMPTY_LIST, servers: [GAMMA_PRIMARY] }),
    );
    visit("");
    const group = await screen.findByTestId("remote-group-gamma");
    expect(within(group).getByText("primary")).toBeInTheDocument();
    expect(within(group).queryByRole("button", { name: "Set default" })).toBeNull();
    expect(within(group).getByRole("button", { name: "Add another account" })).toBeInTheDocument();
  });

  it("Set default POSTs to /{id}/default and reloads the list", async () => {
    const calls = recording(undefined, { ...EMPTY_LIST, servers: [GAMMA_PRIMARY, GAMMA_WORK] });
    visit("");

    const group = await screen.findByTestId("remote-group-gamma");
    const listGets = () =>
      calls.filter((c) => c.url === "/api/remote-mcp-servers" && (c.init?.method ?? "GET") === "GET")
        .length;
    const before = listGets();
    fireEvent.click(within(group).getByRole("button", { name: "Set default" }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) => c.url === "/api/remote-mcp-servers/g2/default" && c.init?.method === "POST",
        ),
      ).toBe(true),
    );
    await waitFor(() => expect(listGets()).toBeGreaterThan(before));
  });

  it("Rename PUTs the new label to /{id}/account", async () => {
    const calls = recording(undefined, { ...EMPTY_LIST, servers: [GAMMA_PRIMARY, GAMMA_WORK] });
    visit("");

    const group = await screen.findByTestId("remote-group-gamma");
    fireEvent.click(within(group).getAllByRole("button", { name: "Rename" })[1]);
    const input = within(group).getByLabelText("New account label for gamma (work)");
    fireEvent.change(input, { target: { value: "Client B" } });
    fireEvent.click(within(group).getByRole("button", { name: "Save label" }));
    await waitFor(() => {
      const put = calls.find(
        (c) => c.url === "/api/remote-mcp-servers/g2/account" && c.init?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String(put?.init?.body))).toEqual({ account: "Client B" });
    });
  });

  it("Add another account requires a label and POSTs it (OAuth: no auth key, sign-in prompt)", async () => {
    let posted: Record<string, unknown> | null = null;
    recording(
      (body) => {
        posted = body;
        return { status: 200, body: { id: "g3", name: "gamma", account: "personal" } };
      },
      { ...EMPTY_LIST, servers: [GAMMA_PRIMARY] },
    );
    visit("");

    const group = await screen.findByTestId("remote-group-gamma");
    fireEvent.click(within(group).getByTestId("add-seat-gamma"));
    const submit = within(group).getByTestId("add-seat-submit-gamma") as HTMLButtonElement;
    // No label, no add.
    expect(submit.disabled).toBe(true);
    fireEvent.change(within(group).getByLabelText("Account label for the new gamma login"), {
      target: { value: "personal" },
    });
    expect(submit.disabled).toBe(false);
    fireEvent.click(submit);

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({
      name: "gamma",
      url: "https://mcp.gamma.app/mcp",
      account: "personal",
    });
    // OAuth seats omit `auth` (the backend's discovery default) and get the
    // post-add sign-in prompt so the login can start right away.
    expect(posted).not.toHaveProperty("auth");
    expect(posted).not.toHaveProperty("api_key");
    expect(
      await screen.findByRole("dialog", { name: "Sign in to gamma (personal)?" }),
    ).toBeInTheDocument();
  });

  it("Add another account for an api_key connection also takes the key and reuses the manifest's key transport", async () => {
    let posted: Record<string, unknown> | null = null;
    recording(
      (body) => {
        posted = body;
        return { status: 200, body: { id: "b2", tool_count: 6 } };
      },
      { ...EMPTY_LIST, servers: [BB_PRIMARY] },
    );
    visit("");

    const group = await screen.findByTestId("remote-group-browserbase");
    fireEvent.click(within(group).getByTestId("add-seat-browserbase"));
    const submit = within(group).getByTestId("add-seat-submit-browserbase") as HTMLButtonElement;
    fireEvent.change(within(group).getByLabelText("Account label for the new browserbase login"), {
      target: { value: "work" },
    });
    // A label alone is not enough for an api_key seat — the key is required too.
    expect(submit.disabled).toBe(true);
    fireEvent.change(within(group).getByLabelText("API key for the new browserbase login"), {
      target: { value: "bb_second_key" },
    });
    fireEvent.click(submit);

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({
      name: "browserbase",
      url: BROWSERBASE.url,
      auth: "api_key",
      account: "work",
      api_key: "bb_second_key",
      api_key_query: "browserbaseApiKey",
    });
  });

  it("an added directory entry offers Add another account through its guided form", async () => {
    let posted: Record<string, unknown> | null = null;
    recording(
      (body) => {
        posted = body;
        return { status: 200, body: { id: "b2", tool_count: 6 } };
      },
      { ...EMPTY_LIST, servers: [BB_PRIMARY] },
    );
    visit("");

    // Browserbase is featured, so the unfiltered directory renders its card
    // twice (Featured shelf + category group); either copy will do.
    const card = (await screen.findAllByTestId("dir-card-browserbase"))[0];
    // The primary Add is spent ("Added"); the second affordance opens the form.
    expect((within(card).getByTestId("dir-add-browserbase") as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(within(card).getByTestId("dir-add-account-browserbase"));
    await within(card).findByTestId("dir-form-browserbase");
    const add = within(card).getByTestId("dir-form-add-browserbase") as HTMLButtonElement;
    fireEvent.change(
      within(card).getByPlaceholderText("paste your key (stored encrypted, never shown again)"),
      { target: { value: "bb_third_key" } },
    );
    // Key alone is not enough: the second login needs its label.
    expect(add.disabled).toBe(true);
    fireEvent.change(within(card).getByTestId("dir-form-account-browserbase"), {
      target: { value: "personal" },
    });
    expect(add.disabled).toBe(false);
    fireEvent.click(add);

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({
      name: "browserbase",
      auth: "api_key",
      account: "personal",
      api_key: "bb_third_key",
      api_key_query: "browserbaseApiKey",
    });
  });

  it("shows the owner's account label on shared rows, without Set default", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch(undefined, CATALOG, {
        ...EMPTY_LIST,
        shared_with_me: [
          { ...GAMMA_WORK, id: "s1", owner: "ann@example.com" },
          { ...GAMMA_WORK, id: "s2", account: "", owner: "bob@example.com" },
        ],
      }),
    );
    visit("");
    await screen.findByText("shared by ann@example.com");
    expect(screen.getByText("work")).toBeInTheDocument();
    expect(screen.getByText("primary")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Set default" })).toBeNull();
  });
});
