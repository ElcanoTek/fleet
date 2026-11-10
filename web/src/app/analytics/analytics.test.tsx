import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { AnalyticsPage } from "./page";

let mockRole = "admin";
let mockSignedIn = true;

vi.mock("@/app/shared/hooks/useOrchestratorSession", () => ({
  useOrchestratorSession: () => ({
    ready: true,
    signedIn: mockSignedIn,
    username: "admin@example.com",
    role: mockRole,
    noAccess: false,
    unreachable: false,
    logout: async () => {},
  }),
}));

vi.mock("@/app/lib/useClientConfig", () => ({
  useClientConfig: () => ({ branding: { app_name: "Fleet", logo_url: "" } }),
}));

vi.mock("@/app/shared/ui/NavRail", () => ({
  NavRail: ({ children }: { children?: ReactNode }) => <nav data-testid="nav-rail">{children}</nav>,
  useRailCollapse: () => ({
    collapsed: false,
    isNarrow: false,
    railReady: true,
    setCollapsed: () => {},
  }),
}));

vi.mock("@/app/orchestrator/UsagePanel", () => ({
  UsagePanel: () => <div data-testid="usage-panel-stub">Usage Panel</div>,
}));

vi.mock("@/app/orchestrator/AdoptionPanel", () => ({
  AdoptionPanel: () => <div data-testid="adoption-panel-stub">Adoption Panel</div>,
}));

const usageMock = vi.fn();
const tasksMock = vi.fn();

vi.mock("@/app/shared/lib/orchestratorApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/app/shared/lib/orchestratorApi")>();
  return {
    ...actual,
    orchestratorApi: {
      ...actual.orchestratorApi,
      usage: (...args: unknown[]) => usageMock(...args),
      tasks: (...args: unknown[]) => tasksMock(...args),
    },
  };
});

const SAMPLE_MODEL_REPORT = {
  group_by: "model" as const,
  from: "2026-11-01T00:00:00Z",
  to: "2026-11-10T00:00:00Z",
  sources: ["tasks", "chat"],
  note: "Dollar figures cover only runs with model pricing available.",
  buckets: [
    {
      key: "openrouter/z-ai/glm-5.1",
      label: "",
      cost_usd: 0,
      task_cost_usd: 0,
      chat_cost_usd: 0,
      prompt_tokens: 8_000_000,
      completion_tokens: 2_000_000,
      cached_tokens: 6_000_000,
      uncached_tokens: 2_000_000,
      cache_hit_rate_pct: 75.0,
      task_iterations: 120,
      chat_turns: 340,
    },
    {
      key: "anthropic/claude-opus-4-6",
      label: "",
      cost_usd: 42.5,
      task_cost_usd: 30.0,
      chat_cost_usd: 12.5,
      prompt_tokens: 1_500_000,
      completion_tokens: 500_000,
      cached_tokens: 1_200_000,
      uncached_tokens: 300_000,
      cache_hit_rate_pct: 80.0,
      task_iterations: 45,
      chat_turns: 25,
    },
  ],
  totals: {
    key: "TOTAL",
    cost_usd: 42.5,
    task_cost_usd: 30.0,
    chat_cost_usd: 12.5,
    prompt_tokens: 9_500_000,
    completion_tokens: 2_500_000,
    cached_tokens: 7_200_000,
    uncached_tokens: 2_300_000,
    cache_hit_rate_pct: 75.8,
    task_iterations: 165,
    chat_turns: 365,
  },
};

const SAMPLE_ADMIN_STATS = {
  users: [
    {
      email: "alice@example.com",
      conversation_count: 14,
      pinned_count: 2,
      last_activity: 1700000000,
      total_cost_usd: 18.25,
      total_turns: 90,
      total_prompt_tokens: 1_200_000,
      total_cached_tokens: 900_000,
      uncached_prompt_tokens: 300_000,
      cache_hit_rate_pct: 75.0,
      frustrated_messages: 3,
      struggle_signals: 2,
      advanced_switches: 1,
    },
  ],
  top_conversations: [
    {
      conversation_id: "11111111-2222-3333-4444-555555555555",
      title: "Patrol: Tech HQ & Fleet Production Watch",
      user_email: "brad@example.com",
      model: "openrouter/z-ai/glm-5.1",
      turns: 240,
      prompt_tokens: 5_000_000,
      completion_tokens: 500_000,
      cached_tokens: 4_000_000,
      cost_usd: 0,
      frustrated_messages: 0,
      struggle_signals: 1,
      advanced_switches: 0,
      updated_at: 1700000500,
    },
  ],
  top_tasks: [
    {
      task_id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
      title: "Morning Executive Brief",
      owner_email: "jeanne@example.com",
      status: "success",
      model: "anthropic/claude-opus-4-6",
      recurrence: "daily",
      iterations: 18,
      prompt_tokens: 450_000,
      completion_tokens: 50_000,
      cost_usd: 4.2,
    },
  ],
};

beforeEach(() => {
  mockRole = "admin";
  mockSignedIn = true;
  window.history.replaceState(null, "", "/analytics");
  usageMock.mockReset();
  usageMock.mockResolvedValue(SAMPLE_MODEL_REPORT);
  tasksMock.mockReset();

  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = typeof input === "string" ? input : (input as Request).url;
    if (url.endsWith("/api/admin/settings") && (!init?.method || init.method === "GET")) {
      return new Response(
        JSON.stringify({
          settings: [
            {
              key: "monthly_soft_token_limit",
              value: "20000000",
              default_value: "0",
              source: "admin",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    if (url.endsWith("/api/admin/settings/monthly_soft_token_limit") && init?.method === "PUT") {
      return new Response(JSON.stringify({ ok: true }), { status: 200 });
    }
    if (url.endsWith("/api/admin/stats")) {
      return new Response(JSON.stringify(SAMPLE_ADMIN_STATS), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response("{}", { status: 200 });
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("AnalyticsPage", () => {
  it("renders monthly allowance banner and splits Included ($0) vs Billed models", async () => {
    render(<AnalyticsPage />);

    const banner = await screen.findByTestId("analytics-monthly-banner");
    await waitFor(() => {
      expect(within(banner).getByTestId("monthly-total-tokens").textContent).toBe("12M");
    });
    expect(within(banner).getByTestId("monthly-included-share").textContent).toBe("83.3%");
    expect(within(banner).getByTestId("monthly-billed-spend").textContent).toBe("$42.50");
    expect(within(banner).getByText(/60\.0%/)).toBeTruthy();

    const table = await screen.findByTestId("model-breakdown-table");
    const rows = within(table).getAllByTestId("model-breakdown-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("openrouter/z-ai/glm-5.1");
    expect(rows[0].textContent).toContain("Included ($0)");
    expect(rows[1].textContent).toContain("anthropic/claude-opus-4-6");
    expect(rows[1].textContent).toContain("Billed");
  });

  it("allows editing the monthly soft token limit inline", async () => {
    render(<AnalyticsPage />);
    const editBtn = await screen.findByTestId("edit-monthly-soft-limit");
    fireEvent.click(editBtn);

    const input = await screen.findByTestId("monthly-soft-limit-input");
    fireEvent.change(input, { target: { value: "50000000" } });
    fireEvent.click(screen.getByTestId("save-monthly-soft-limit"));

    await waitFor(() => {
      expect(globalThis.fetch).toHaveBeenCalledWith(
        "/api/admin/settings/monthly_soft_token_limit",
        expect.objectContaining({
          method: "PUT",
          body: JSON.stringify({ value: "50000000" }),
        }),
      );
    });
  });

  it("renders Outliers & Friction tab with top chats, heavy tasks, and user struggle signals", async () => {
    render(<AnalyticsPage />);

    fireEvent.click(await screen.findByRole("tab", { name: /Outliers & Friction/i }));

    const convTable = await screen.findByTestId("outliers-conversations-table");
    const convLink = within(convTable).getByText("Patrol: Tech HQ & Fleet Production Watch");
    expect(convLink).toBeTruthy();
    expect(convLink.closest("a")?.getAttribute("href")).toBe("/chat?c=11111111-2222-3333-4444-555555555555");
    expect(within(convTable).getByText("Patrol")).toBeTruthy();

    const taskTable = await screen.findByTestId("outliers-tasks-table");
    expect(within(taskTable).getByText("Morning Executive Brief")).toBeTruthy();
    expect(within(taskTable).getByText("jeanne@example.com")).toBeTruthy();

    const userTable = await screen.findByTestId("outliers-user-signals-table");
    const userRow = within(userTable).getByTestId("outliers-user-signal-row");
    expect(userRow.textContent).toContain("alice@example.com");
    expect(userRow.textContent).toContain("$18.25");
  });

  it("honors ?tab=adoption and ?tab=usage deep links", async () => {
    window.history.replaceState(null, "", "/analytics?tab=adoption");
    render(<AnalyticsPage />);

    expect(await screen.findByTestId("adoption-panel-stub")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "Usage" }));
    expect(await screen.findByTestId("usage-panel-stub")).toBeTruthy();
  });

  it("blocks non-admins with an Admin access required view", async () => {
    mockRole = "client";
    render(<AnalyticsPage />);

    expect(await screen.findByTestId("analytics-admin-required")).toBeTruthy();
  });
});
