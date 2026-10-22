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
    expect(screen.queryByPlaceholderText("e.g. 200000")).toBeNull();
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
    fireEvent.change(windowInput, { target: { value: "200k" } });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    expect(await screen.findByText("Context window must be a whole number of tokens.")).toBeInTheDocument();
    expect(posts()).toHaveLength(0);

    fireEvent.change(windowInput, { target: { value: "200000" } });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    const body = JSON.parse(String((posts()[0][1] as RequestInit).body));
    expect(body).toMatchObject({ name: "anthropic-direct", type: "anthropic", context_window_tokens: 200000 });
  });

  it("keeps the declared context window when a row is disabled or enabled", async () => {
    const fetchMock = mockFetch([ROW]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersAdminPage />);
    fireEvent.click(await screen.findByRole("button", { name: "Disable" }));
    await waitFor(() => {
      const put = fetchMock.mock.calls.find(([, init]) => init && (init as RequestInit).method === "PUT");
      expect(put).toBeTruthy();
      const body = JSON.parse(String((put![1] as RequestInit).body));
      expect(body).toMatchObject({ enabled: false, context_window_tokens: 200000 });
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
