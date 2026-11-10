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

let mockPinnedModels: Array<{
  slug: string;
  label?: string;
  free?: boolean;
  inputPricePerMtok?: number;
  outputPricePerMtok?: number;
}> = [];

vi.mock("@/app/lib/useClientConfig", () => ({
  useClientConfig: () => ({
    branding: { app_name: "Fleet", logo_url: "" },
    models: { pinned: mockPinnedModels },
  }),
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
  mockPinnedModels = [];
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
              key: "monthly_soft_spend_limit_usd",
              value: "100",
              default_value: "0",
              source: "admin",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    if (url.endsWith("/api/admin/settings/monthly_soft_spend_limit_usd") && init?.method === "PUT") {
      return new Response(JSON.stringify({ ok: true }), { status: 200 });
    }
    if (url.endsWith("/api/admin/stats")) {
      return new Response(JSON.stringify(SAMPLE_ADMIN_STATS), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.endsWith("/api/admin/server-stats")) {
      return new Response(
        JSON.stringify({
          available: true,
          sampled_at: "2026-11-10T00:00:00Z",
          hostname: "fleet-box",
          platform: "linux/amd64",
          uptime_seconds: 86400,
          cpu: { available: true, cores: 8, usage_percent: 25.0, load_1: 0.4, load_5: 0.3, load_15: 0.2 },
          memory: { available: true, total_bytes: 8 * 1024 ** 3, used_bytes: 2 * 1024 ** 3, available_bytes: 6 * 1024 ** 3, swap_total_bytes: 0, swap_used_bytes: 0 },
          disk: { available: true, path: "/", total_bytes: 100 * 1024 ** 3, used_bytes: 20 * 1024 ** 3, available_bytes: 80 * 1024 ** 3, usage_percent: 20 },
          network: { available: true, interfaces: 1, received_bytes: 1024, transmitted_bytes: 1024, receive_bytes_per_second: 100, transmit_bytes_per_second: 100 },
          agents: {
            active_agents_total: 4,
            active_chat_turns: 1,
            running_tasks: 1,
            queued_tasks: 2,
            completed_tasks_today: 12,
            failed_tasks_today: 0,
            active_subagents: 2,
            subagents_spawned_24h: 10,
            subagents_spawned_total: 50,
            tool_calls_24h: 300,
            tool_calls_total: 2000,
            active_conversations_24h: 15,
            llm_calls_today: 60,
            llm_cost_today_usd: 9.5,
            sandbox_pool_size: 4,
            sandbox_pool_available: 4,
            db_pool_in_use: 1,
            db_pool_idle: 4,
            db_pool_size: 5,
            mcp_servers_enabled: 10,
            mcp_servers_total: 12,
            goroutines: 48,
            process_memory_mb: 96.0,
            fleet_version: "v2026.11.10.6",
          },
          warnings: [],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
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
    expect(within(banner).getByText(/of \$100\.00 soft spend limit \(42\.5%\)/)).toBeTruthy();

    const table = await screen.findByTestId("model-breakdown-table");
    const rows = within(table).getAllByTestId("model-breakdown-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("openrouter/z-ai/glm-5.1");
    expect(rows[0].textContent).toContain("Included ($0)");
    expect(rows[1].textContent).toContain("anthropic/claude-opus-4-6");
    expect(rows[1].textContent).toContain("Billed");
    expect(screen.getByTestId("model-attribution-note").textContent).toContain(
      "(deleted chats & default-model turns)",
    );
  });

  it("treats pinned free:true models as Included ($0) even when historical cost_usd > 0 and labels empty key clearly", async () => {
    mockPinnedModels = [
      { slug: "qwen/qwen3.6-plus", label: "Custom Zero Model", free: true },
    ];
    usageMock.mockResolvedValue({
      ...SAMPLE_MODEL_REPORT,
      buckets: [
        {
          key: "openrouter/qwen/qwen3.6-plus",
          label: "",
          cost_usd: 15.0,
          task_cost_usd: 5.0,
          chat_cost_usd: 10.0,
          prompt_tokens: 6_000_000,
          completion_tokens: 2_000_000,
          cached_tokens: 4_000_000,
          uncached_tokens: 2_000_000,
          cache_hit_rate_pct: 66.7,
          task_iterations: 20,
          chat_turns: 80,
        },
        {
          key: "",
          label: "",
          cost_usd: 0,
          task_cost_usd: 0,
          chat_cost_usd: 0,
          prompt_tokens: 1_500_000,
          completion_tokens: 500_000,
          cached_tokens: 500_000,
          uncached_tokens: 1_000_000,
          cache_hit_rate_pct: 33.3,
          task_iterations: 0,
          chat_turns: 40,
        },
      ],
      totals: {
        ...SAMPLE_MODEL_REPORT.totals,
        cost_usd: 15.0,
        task_cost_usd: 5.0,
        chat_cost_usd: 10.0,
        prompt_tokens: 7_500_000,
        completion_tokens: 2_500_000,
      },
    });

    render(<AnalyticsPage />);

    const banner = await screen.findByTestId("analytics-monthly-banner");
    await waitFor(() => {
      expect(within(banner).getByTestId("monthly-billed-spend").textContent).toBe("$0.00");
    });
    expect(within(banner).getByTestId("monthly-included-share").textContent).toBe("80.0%");
    expect(within(banner).getByText(/of \$100\.00 soft spend limit \(0\.0%\)/)).toBeTruthy();

    const table = await screen.findByTestId("model-breakdown-table");
    const rows = within(table).getAllByTestId("model-breakdown-row");
    expect(rows[0].textContent).toContain("Custom Zero Model (openrouter/qwen/qwen3.6-plus)");
    expect(rows[0].textContent).toContain("Included ($0)");
    expect(rows[0].textContent).toContain("$0.00");
    expect(rows[1].textContent).toContain("(deleted chats & default-model turns)");
    expect(rows[1].textContent).toContain("No price data");
  });

  it("allows editing the monthly soft spend limit inline", async () => {
    render(<AnalyticsPage />);
    const editBtn = await screen.findByTestId("edit-monthly-soft-limit");
    fireEvent.click(editBtn);

    const input = await screen.findByTestId("monthly-soft-limit-input");
    fireEvent.change(input, { target: { value: "$500" } });
    fireEvent.click(screen.getByTestId("save-monthly-soft-limit"));

    await waitFor(() => {
      expect(globalThis.fetch).toHaveBeenCalledWith(
        "/api/admin/settings/monthly_soft_spend_limit_usd",
        expect.objectContaining({
          method: "PUT",
          body: JSON.stringify({ value: "500" }),
        }),
      );
    });
  });

  it("renders Problem Chats & Friction tab with top chats, heavy tasks, and user struggle signals", async () => {
    render(<AnalyticsPage />);

    fireEvent.click(await screen.findByRole("tab", { name: /Problem Chats & Friction/i }));

    const convTable = await screen.findByTestId("outliers-conversations-table");
    expect(screen.getByTestId("friction-signals-explainer").textContent).toContain(
      "How Friction & Escalation Signals Work",
    );
    const convLink = within(convTable).getByText("Patrol: Tech HQ & Fleet Production Watch");
    expect(convLink).toBeTruthy();
    expect(convLink.closest("a")?.getAttribute("href")).toBe("/chat?c=11111111-2222-3333-4444-555555555555");
    expect(within(convTable).getByText("Patrol")).toBeTruthy();
    expect(within(convTable).getByText("0 frustrated · 1 struggles · 0 upgrades")).toBeTruthy();

    const taskTable = await screen.findByTestId("outliers-tasks-table");
    expect(within(taskTable).getByText("Morning Executive Brief")).toBeTruthy();
    expect(within(taskTable).getByText("jeanne@example.com")).toBeTruthy();

    const userTable = await screen.findByTestId("outliers-user-signals-table");
    const userRow = within(userTable).getByTestId("outliers-user-signal-row");
    expect(userRow.textContent).toContain("alice@example.com");
    expect(userRow.textContent).toContain("$18.25");
  });

  it("honors ?tab=adoption, ?tab=usage, and ?tab=server deep links", async () => {
    window.history.replaceState(null, "", "/analytics?tab=adoption");
    render(<AnalyticsPage />);

    expect(await screen.findByTestId("adoption-panel-stub")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "Spend & Budgets" }));
    expect(await screen.findByTestId("usage-panel-stub")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "Server & Live Agents" }));
    expect(await screen.findByTestId("analytics-server-tab")).toBeTruthy();
  });

  it("blocks non-admins with an Admin access required view", async () => {
    mockRole = "client";
    render(<AnalyticsPage />);

    expect(await screen.findByTestId("analytics-admin-required")).toBeTruthy();
  });
});
