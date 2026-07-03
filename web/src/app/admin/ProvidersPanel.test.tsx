import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ProvidersPanel, type LLMProvider } from "./ProvidersPanel";

// ProvidersPanel — the admin surface for admin-managed LLM providers. The
// load-bearing assertions: rows render with honest key/enabled status chips
// (never a key value), the add form validates before any network call, and a
// create POSTs the write-only key exactly once.

const ROW: LLMProvider = {
  id: "p1",
  name: "anthropic-direct",
  type: "anthropic",
  base_url: "",
  models: ["claude-opus-4-8"],
  enabled: true,
  has_api_key: true,
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

describe("ProvidersPanel", () => {
  it("lists providers with status chips and no key material", async () => {
    vi.stubGlobal("fetch", mockFetch([ROW]));
    render(<ProvidersPanel />);
    expect(await screen.findByText("anthropic-direct")).toBeInTheDocument();
    expect(screen.getByText("Key stored")).toBeInTheDocument();
    expect(screen.getByText("Enabled")).toBeInTheDocument();
    expect(screen.getByText(/1 model/)).toBeInTheDocument();
  });

  it("validates the draft before any mutation request", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    render(<ProvidersPanel />);
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
    render(<ProvidersPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
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
      expect(body).toMatchObject({ name: "openrouter-team", type: "openrouter", api_key: "sk-or-secret" });
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
    render(<ProvidersPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "Test" }));
    const result = await screen.findByTestId(`probe-result-${ROW.id}`);
    expect(result.textContent).toContain("connected — 12 models served");
    expect(result.textContent).toContain("245ms");
  });

  it("requires explicit confirmation for an enabled catch-all (empty models)", async () => {
    const fetchMock = mockFetch([]);
    vi.stubGlobal("fetch", fetchMock);
    const confirmSpy = vi.fn().mockReturnValue(false);
    vi.stubGlobal("confirm", confirmSpy);
    render(<ProvidersPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByPlaceholderText("my-provider"), {
      target: { value: "fallback" },
    });
    fireEvent.change(screen.getByPlaceholderText("sk-…"), {
      target: { value: "sk-or-x" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create provider" }));
    expect(confirmSpy).toHaveBeenCalledOnce();
    // Declined → no POST fired.
    expect(fetchMock.mock.calls.filter(([, init]) => init && (init as RequestInit).method === "POST")).toHaveLength(0);
  });
});
