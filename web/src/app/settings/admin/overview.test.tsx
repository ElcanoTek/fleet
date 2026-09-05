import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import AdminOverviewPage from "./page";

// Settings → Admin → Overview: system health (GET /api/admin/health-summary)
// + usage aggregates (GET /api/admin/stats) on one page.
//
// Load-bearing MCP assertion carried over from the old HealthPanel test: the
// chips convey the Optional-MCP catalog's enabled-by-default flag, NOT server
// health (the endpoint doesn't probe servers). A chip must never take a danger
// tint, which reads as "broken"; the enabled/optional distinction rides the
// per-chip tooltip. Regression guard for the report that the pills "show up
// red which reads as broken".

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn() }),
}));

// Admin gate: visibility-only; force "admin" so the page renders. (The real
// hook probes an admin endpoint; authorization stays server-side regardless.)
// `adminState` is swapped by the one test that exercises the gate's own
// fallback rendering.
let adminState = "admin";
const retryAdminProbe = vi.fn(() => Promise.resolve(adminState));
vi.mock("../useIsAdmin", () => ({
  useIsAdmin: () => adminState,
  retryAdminProbe: () => retryAdminProbe(),
}));

const SUMMARY = {
  fleet_version: "9.9.9-test",
  uptime_seconds: 3661,
  db: { chat: "healthy", pool_size: 5, in_use: 1, idle: 4 },
  workers: { queued_tasks: 3, running_tasks: 2, completed_today: 41, failed_today: 1 },
  llm: { calls_today: 120, cost_today_usd: 4.5, avg_cost_per_call: 0.0375 },
  mcp_servers: [
    { name: "on-by-default-server", enabled: true },
    { name: "optional-server", enabled: false },
  ],
  conversations_active: 2,
  sandbox_pool: { size: 3, available: 1 },
  disk: {
    available: true,
    path: "/var/lib/fleet",
    total_bytes: 107374182400,
    free_bytes: 64424509440,
    free_percent: 60,
    min_free_percent: 5,
    shedding: false,
  },
  memory_mb: 256,
  goroutines: 73,
};

const NOW = Math.floor(Date.now() / 1000);
const STATS = {
  users: [
    {
      email: "sam@x.com",
      conversation_count: 3,
      pinned_count: 2,
      last_activity: NOW - 7200,
      total_cost_usd: 4.96,
      total_turns: 3,
    },
    {
      email: "brad@x.com",
      conversation_count: 1,
      pinned_count: 1,
      last_activity: NOW - 14400,
      total_cost_usd: 0.11,
      total_turns: 2,
    },
  ],
};

function mockFetch({ summary = SUMMARY as unknown, healthStatus = 200 } = {}) {
  return vi.fn().mockImplementation(async (url: string) => {
    if (String(url).startsWith("/api/admin/health-summary")) {
      return { ok: healthStatus === 200, status: healthStatus, json: async () => summary };
    }
    if (String(url).startsWith("/api/admin/stats")) {
      return { ok: true, status: 200, json: async () => STATS };
    }
    throw new Error(`unexpected ${url}`);
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  adminState = "admin";
});

describe("AdminOverviewPage admin gate", () => {
  it("renders a retryable notice, not a blank page, when the permission probe fails", () => {
    vi.stubGlobal("fetch", mockFetch());
    adminState = "unavailable";
    render(<AdminOverviewPage />);
    // Nothing admin-only is fetched or shown…
    expect(screen.queryByTestId("health-panel-loading")).toBeNull();
    const notice = screen.getByTestId("admin-gate-unavailable");
    expect(notice).toHaveTextContent("Couldn’t check your permissions");
    // …but the reader has a way forward.
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retryAdminProbe).toHaveBeenCalled();
  });

  it("still renders nothing while the probe is unresolved", () => {
    vi.stubGlobal("fetch", mockFetch());
    adminState = "unknown";
    const { container } = render(<AdminOverviewPage />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("AdminOverviewPage system health", () => {
  it("shows the loading state, then the health cards and DB badge", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<AdminOverviewPage />);

    expect(screen.getByTestId("health-panel-loading")).toBeInTheDocument();

    const panel = await screen.findByTestId("health-panel");
    expect(panel).toHaveTextContent("chat DB healthy");
    expect(panel).toHaveTextContent("9.9.9-test"); // version
    expect(panel).toHaveTextContent("up 1h 1m"); // uptime sub
    expect(panel).toHaveTextContent("1/5"); // DB pool in_use/pool_size
    expect(panel).toHaveTextContent("$4.50"); // LLM spend today
    expect(panel).toHaveTextContent("120 calls · $0.037/call");
    expect(panel).toHaveTextContent("2 running"); // tasks
    expect(panel).toHaveTextContent("41 ✓"); // tasks today
    expect(panel).toHaveTextContent("1/3"); // sandbox pool available/size
    expect(panel).toHaveTextContent("256 MB");
    expect(panel).toHaveTextContent("73 goroutines");
    expect(panel).toHaveTextContent("60.0% free"); // disk headroom
  });

  it("spells out that a shedding disk has paused scheduled tasks", async () => {
    // The one disk state a percentage alone would not communicate: the box has
    // already stopped claiming scheduled work, and because chat keeps serving,
    // nothing else on this page would say so.
    vi.stubGlobal(
      "fetch",
      mockFetch({
        summary: {
          ...SUMMARY,
          disk: { ...SUMMARY.disk, free_percent: 2, shedding: true },
        },
      }),
    );
    render(<AdminOverviewPage />);

    const panel = await screen.findByTestId("health-panel");
    expect(panel).toHaveTextContent("2.0% free");
    expect(panel).toHaveTextContent("below the 5% floor — scheduled tasks paused");
  });

  it("reports an unmeasurable disk without claiming a number", async () => {
    vi.stubGlobal(
      "fetch",
      mockFetch({
        summary: {
          ...SUMMARY,
          disk: { ...SUMMARY.disk, available: false, shedding: false, error: "statfs: permission denied" },
        },
      }),
    );
    render(<AdminOverviewPage />);

    const panel = await screen.findByTestId("health-panel");
    expect(panel).toHaveTextContent("statfs: permission denied");
  });

  it("omits the disk card when no guard is wired", async () => {
    vi.stubGlobal("fetch", mockFetch({ summary: { ...SUMMARY, disk: null } }));
    render(<AdminOverviewPage />);

    const panel = await screen.findByTestId("health-panel");
    expect(panel).not.toHaveTextContent("% free");
  });

  it("falls back to a single unavailable Tasks card when workers is null", async () => {
    vi.stubGlobal("fetch", mockFetch({ summary: { ...SUMMARY, workers: null } }));
    render(<AdminOverviewPage />);

    const panel = await screen.findByTestId("health-panel");
    expect(panel).toHaveTextContent("scheduler stats unavailable");
    expect(panel).not.toHaveTextContent("Tasks today");
  });

  it("surfaces a failed health fetch as the error banner", async () => {
    vi.stubGlobal("fetch", mockFetch({ healthStatus: 500 }));
    render(<AdminOverviewPage />);

    const banner = await screen.findByTestId("health-panel-error");
    expect(banner).toHaveTextContent("Health unavailable: health request failed: 500");
  });
});

describe("AdminOverviewPage MCP chips", () => {
  it("keeps the enabled/optional distinction in tooltips, never a danger tint", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<AdminOverviewPage />);

    const optional = await screen.findByText("optional-server");
    const onByDefault = await screen.findByText("on-by-default-server");

    // The danger tone (which reads as "broken") must not be used for a
    // perfectly-normal optional server — nor for any chip: the endpoint
    // doesn't health-probe servers.
    for (const chip of [optional, onByDefault]) {
      expect(chip.className).not.toContain("--color-danger");
    }
    // Tooltips carry what the catalog actually knows (default-on vs optional).
    expect(optional.getAttribute("title")).toMatch(/optional/i);
    expect(onByDefault.getAttribute("title")).toMatch(/default/i);
  });
});

describe("AdminOverviewPage usage", () => {
  it("aggregates the per-user stats into the Usage cards", async () => {
    vi.stubGlobal("fetch", mockFetch());
    render(<AdminOverviewPage />);
    await screen.findByTestId("health-panel");

    expect(screen.getByText("Users")).toBeInTheDocument();
    expect(await screen.findByText("2h ago")).toBeInTheDocument(); // most recent (first row)
    expect(screen.getByText("5")).toBeInTheDocument(); // turns total 3 + 2
    expect(screen.getByText("$5.07")).toBeInTheDocument(); // spend total 4.96 + 0.11
  });

  it("refetches BOTH health and stats on manual refresh", async () => {
    const fetchMock = mockFetch();
    vi.stubGlobal("fetch", fetchMock);
    render(<AdminOverviewPage />);
    await screen.findByTestId("health-panel");

    const countCalls = (prefix: string) =>
      fetchMock.mock.calls.filter((c) => String(c[0]).startsWith(prefix)).length;
    const healthBefore = countCalls("/api/admin/health-summary");
    const statsBefore = countCalls("/api/admin/stats");

    fireEvent.click(screen.getByRole("button", { name: "Refresh now" }));

    await waitFor(() => {
      expect(countCalls("/api/admin/health-summary")).toBe(healthBefore + 1);
      expect(countCalls("/api/admin/stats")).toBe(statsBefore + 1);
    });
  });
});
