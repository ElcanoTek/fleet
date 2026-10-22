import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import ProvidersAdminPage, { type LLMProvider } from "./page";

// Settings → Admin → Providers — the admin surface for admin-managed LLM
// providers. The load-bearing assertions: rows render with an honest
// type/models/disabled sub line (never a key value), the add form validates
// before any network call, a create POSTs the write-only key exactly once,
// the connection test renders its key-free result, and Remove requires the
// inline two-click confirmation before the DELETE fires.

vi.mock("@/app/lib/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; force "admin" so the page renders. (The real
// hook probes an admin endpoint; authorization stays server-side regardless.)
vi.mock("../../useIsAdmin", () => ({
  useIsAdmin: () => "admin",
}));

const ROW: LLMProvider = {
  id: "p1",
  name: "anthropic-direct",
  type: "anthropic",
  base_url: "",
  models: ["claude-opus-4-8"],
  enabled: true,
  has_api_key: true,
  context_window_tokens: 200000,
  created_at: 1,
  updated_at: 1,
};

function mockFetch(providers: LLMProvider[]) {
  return vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
    if (!init || init.method === undefined || init.method === "GET") {
      return { ok: true, status: 200, json: async () => ({ providers }) };
    }
    return { ok: true, status: 200, json: async () => ({}), text: async () => "{}" };
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("ProvidersAdminPage", () => {
  it("lists providers with an honest sub line and no key material", async () => {
    const disabledCatchAll: LLMProvider = {
      id: "p2",
      name: "local-ollama",
      type: "ollama",
      base_url: "http://localhost:11434/v1",
      models: [],
      enabled: false,
      has_api_key: false,
      context_window_tokens: 65536,
      created_at: 1,
      updated_at: 1,
    };
    vi.stubGlobal("fetch", mockFetch([ROW, disabledCatchAll]));
    render(<ProvidersAdminPage />);
    expect(await screen.findByText("anthropic-direct")).toBeInTheDocument();
    expect(screen.getByText("Anthropic · 1 model · 200,000-token context")).toBeInTheDocument();
    expect(
      screen.getByText("Ollama (local) · catch-all (any model) · 65,536-token context · disabled"),
    ).toBeInTheDocument();
    // Keyless is only flagged for types that need a key (ollama doesn't).
    expect(screen.queryByText("No key")).toBeNull();
  });

  it("flags a keyed provider with no stored key", async () => {
    vi.stubGlobal("fetch", mockFetch([{ ...ROW, has_api_key: false }]));
    render(<ProvidersAdminPage />);
    expect(await screen.findByText("No key")).toBeInTheDocument();
  });

  it("flags a row with no declared context window, but never an OpenRouter row", async () => {
    const openrouter: LLMProvider = {
      ...ROW,
      id: "p5",
      name: "openrouter",
      type: "openrouter",
      models: [],
      context_window_tokens: 0,
    };
    vi.stubGlobal("fetch", mockFetch([{ ...ROW, context_window_tokens: 0 }, openrouter]));
    render(<ProvidersAdminPage />);
    expect(await screen.findByText("Anthropic · 1 model")).toBeInTheDocument();
    expect(screen.getAllByText("No context window")).toHaveLength(1);
  });

  it("requires a context window before enabling a non-OpenRouter provider, then sends it", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    // OpenRouter (the default type) takes its limits from its catalog: no field.
    expect(document.getElementById("providerContextWindow")).toBeNull();
    expect(screen.queryByLabelText(/Context window/)).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("my-provider"), {
      target: { value: "anthropic-direct" },
    });
    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "anthropic" } });
    fireEvent.change(screen.getByPlaceholderText("sk-…"), { target: { value: "sk-ant-x" } });
    fireEvent.change(screen.getByPlaceholderText(/claude-sonnet-4-5/), {
      target: { value: "claude-opus-4-8" },
    });
    const posts = () =>
      fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "POST");
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    expect(await screen.findByText(/Anthropic needs its context window/)).toBeInTheDocument();
    expect(posts()).toHaveLength(0);

    const windowInput = screen.getByPlaceholderText("e.g. 200000");
    expect(windowInput).toHaveAttribute("id", "providerContextWindow");
    // Each refusal replaces the previous message, so every step is observed.
    const refuse = async (value: string, message: RegExp) => {
      fireEvent.change(windowInput, { target: { value } });
      fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
      expect(await screen.findByText(message)).toBeInTheDocument();
      expect(posts()).toHaveLength(0);
    };
    await refuse("0", /must be a positive whole number of tokens/);
    await refuse("3000000000", /must be at most/);
    await refuse("200k", /must be a positive whole number of tokens/);

    // Small windows are allowed but warned about, in two tiers, and the
    // warning is announced and tied to the field.
    fireEvent.change(windowInput, { target: { value: "4096" } });
    expect(screen.getByTestId("provider-small-window")).toHaveTextContent(/usually cannot fit/);
    expect(screen.getByRole("status")).toBe(screen.getByTestId("provider-small-window"));
    expect(windowInput).toHaveAttribute("aria-describedby", "providerContextWindowWarning");
    fireEvent.change(windowInput, { target: { value: "40000" } });
    expect(screen.getByTestId("provider-small-window")).toHaveTextContent(/little room is left/);

    fireEvent.change(windowInput, { target: { value: "200000" } });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    const body = JSON.parse(String((posts()[0][1] as RequestInit).body));
    expect(body).toMatchObject({ name: "anthropic-direct", type: "anthropic", context_window_tokens: 200000 });
  });

  it("keeps the declared context window through Disable, Enable and Edit", async () => {
    const puts = (fetchMock: ReturnType<typeof mockFetch>) =>
      fetchMock.mock.calls
        .filter(([, init]) => init && (init as RequestInit).method === "PUT")
        .map(([, init]) => JSON.parse(String((init as RequestInit).body)));

    let fetchMock = mockFetch([ROW]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Disable" }));
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1));
    expect(puts(fetchMock)[0]).toMatchObject({ enabled: false, context_window_tokens: 200000 });
    cleanup();

    fetchMock = mockFetch([{ ...ROW, enabled: false }]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Enable" }));
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1));
    expect(puts(fetchMock)[0]).toMatchObject({ enabled: true, context_window_tokens: 200000 });
    cleanup();

    fetchMock = mockFetch([ROW]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    expect(document.getElementById("providerContextWindow")).toHaveValue("200000");
    fireEvent.change(screen.getByPlaceholderText(/claude-sonnet-4-5/), {
      target: { value: "claude-opus-4-8\nclaude-sonnet-4-6" },
    });
    fireEvent.click(screen.getByTestId("provider-save"));
    await waitFor(() => expect(puts(fetchMock)).toHaveLength(1));
    expect(puts(fetchMock)[0]).toMatchObject({
      models: ["claude-opus-4-8", "claude-sonnet-4-6"],
      context_window_tokens: 200000,
    });
  });

  it("opens the edit form on the field instead of enabling a row with no context window", async () => {
    const fetchMock = mockFetch([{ ...ROW, enabled: false, context_window_tokens: 0 }]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Enable" }));
    expect(await screen.findByText(/Set anthropic-direct's context window/)).toBeInTheDocument();
    const input = document.getElementById("providerContextWindow");
    expect(input).toHaveValue("");
    expect(document.activeElement).toBe(input);
    expect(screen.getByLabelText("Enabled")).toBeChecked();
    expect(fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "PUT")).toHaveLength(0);
  });

  it("will not save an enabled pre-082 row from Edit until it has a window", async () => {
    const fetchMock = mockFetch([{ ...ROW, context_window_tokens: 0 }]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByTestId("provider-save"));
    expect(await screen.findByText(/Anthropic needs its context window/)).toBeInTheDocument();
    expect(fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "PUT")).toHaveLength(0);
  });

  it("clears a new row's window on a type change and hides it for OpenRouter", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "anthropic" } });
    fireEvent.change(document.getElementById("providerContextWindow")!, { target: { value: "200000" } });
    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "ollama" } });
    expect(document.getElementById("providerContextWindow")).toHaveValue("");
    expect(screen.getByPlaceholderText("the server's num_ctx, e.g. 65536")).toBeInTheDocument();
    fireEvent.change(document.getElementById("providerContextWindow")!, { target: { value: "65536" } });
    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "openrouter" } });
    expect(document.getElementById("providerContextWindow")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("my-provider"), { target: { value: "or-team" } });
    fireEvent.change(screen.getByPlaceholderText("sk-…"), { target: { value: "sk-or-x" } });
    fireEvent.change(screen.getByPlaceholderText(/claude-sonnet-4-5/), { target: { value: "x/y" } });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(([, init]) => init && (init as RequestInit).method === "POST");
      expect(post).toBeTruthy();
      expect(JSON.parse(String((post![1] as RequestInit).body))).toMatchObject({
        type: "openrouter",
        context_window_tokens: 0,
      });
    });
  });

  it("validates the draft before any mutation request", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByPlaceholderText("my-provider"), {
      target: { value: "Bad/Name" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    expect(await screen.findByText(/lowercase slug/)).toBeInTheDocument();
    // Only the initial GET fired — no POST for an invalid draft.
    expect(fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "POST")).toHaveLength(0);
  });

  it("creates a provider, sending the key write-only", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    // Draft-form fields are explicitly label-associated (htmlFor + id).
    expect(screen.getByLabelText("Type")).toHaveAttribute("id", "providerType");
    expect(screen.getByLabelText("Enabled")).toHaveAttribute(
      "id",
      "providerEnabled",
    );
    fireEvent.change(screen.getByPlaceholderText("my-provider"), {
      target: { value: "openrouter-team" },
    });
    fireEvent.change(screen.getByPlaceholderText("sk-…"), {
      target: { value: "sk-or-secret" },
    });
    fireEvent.change(screen.getByPlaceholderText(/claude-sonnet-4-5/), {
      target: { value: "gpt-5.2\n\ngpt-5.2\n" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        ([, init]) => init && (init as RequestInit).method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse(String((post![1] as RequestInit).body));
      // Key is write-only outbound; models are trimmed of blank lines.
      expect(body).toMatchObject({
        name: "openrouter-team",
        type: "openrouter",
        api_key: "sk-or-secret",
        context_window_tokens: 0,
      });
      expect(body.models).toEqual(["gpt-5.2", "gpt-5.2"]);
    });
  });

  it("runs the connection test and renders the key-free result line", async () => {
    const fetchMock = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (String(url).endsWith("/test") && init?.method === "POST") {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            ok: true,
            detail: "connected — 12 models served",
            served_model_count: 12,
            latency_ms: 245,
          }),
        };
      }
      return { ok: true, status: 200, json: async () => ({ providers: [ROW] }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Test" }));
    const result = await screen.findByTestId(`probe-result-${ROW.id}`);
    expect(result.textContent).toContain("connected — 12 models served");
    expect(result.textContent).toContain("245ms");
  });

  it("requires an inline second click for an enabled catch-all (empty models)", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    // No native dialog is involved: a window.confirm call would be a regression.
    const confirmSpy = vi.fn().mockReturnValue(true);
    vi.stubGlobal("confirm", confirmSpy);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByPlaceholderText("my-provider"), {
      target: { value: "fallback" },
    });
    fireEvent.change(screen.getByPlaceholderText("sk-…"), {
      target: { value: "sk-or-x" },
    });
    const posts = () =>
      fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "POST");
    const save = screen.getByTestId("provider-save");
    // First click arms: the warning shows inline, nothing is sent.
    fireEvent.click(save);
    expect(confirmSpy).not.toHaveBeenCalled();
    expect(save).toHaveTextContent("Confirm catch-all");
    expect(screen.getByTestId("provider-catch-all-warning")).toHaveTextContent(/CATCH-ALL/);
    expect(posts()).toHaveLength(0);
    // Escape disarms without submitting.
    fireEvent.keyDown(save, { key: "Escape" });
    expect(save).toHaveTextContent("Create provider");
    expect(posts()).toHaveLength(0);
    // Arm again and confirm → the POST fires.
    fireEvent.click(save);
    fireEvent.click(save);
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it("replaces the loading line with the error and a working Retry when the load fails", async () => {
    let calls = 0;
    const fetchMock = vi.fn().mockImplementation(async () => {
      calls += 1;
      if (calls === 1) throw new TypeError("Failed to fetch");
      return { ok: true, status: 200, json: async () => ({ providers: [ROW] }) };
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    expect(await screen.findByText("Failed to fetch")).toBeInTheDocument();
    // The list never arrived, so "Loading…" would be a lie under the banner.
    expect(screen.queryByText("Loading…")).toBeNull();
    fireEvent.click(screen.getByTestId("providers-retry"));
    expect(await screen.findByText("anthropic-direct")).toBeInTheDocument();
    expect(screen.queryByText("Failed to fetch")).toBeNull();
    expect(screen.queryByTestId("providers-retry")).toBeNull();
  });

  it("removes a provider only after the inline confirm's second click", async () => {
    const fetchMock = mockFetch([ROW]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    const remove = await screen.findByRole("button", { name: "Remove" });
    // First click arms the button; nothing is deleted yet.
    fireEvent.click(remove);
    expect(remove).toHaveTextContent("Confirm remove");
    expect(fetchMock.mock.calls.filter(([, i]) => i && (i as RequestInit).method === "DELETE")).toHaveLength(0);
    // Second click fires the DELETE (the stored key goes with the row).
    fireEvent.click(remove);
    await waitFor(() => {
      const del = fetchMock.mock.calls.find(([, i]) => i && (i as RequestInit).method === "DELETE");
      expect(del?.[0]).toBe("/api/admin/llm-providers/p1");
    });
  });
});
