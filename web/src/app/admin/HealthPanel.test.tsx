import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { HealthPanel } from "./HealthPanel";

// HealthPanel — the MCP pills convey the Optional-MCP catalog's
// enabled-by-default flag, NOT server health (the endpoint doesn't probe
// servers). Load-bearing assertion: an optional (off-by-default) server must
// NOT render with the danger tone, which reads as "broken"; it is neutral.
// An on-by-default server is success. Regression guard for the report that the
// pills "show up red which reads as broken".

const SUMMARY = {
  status: "ok",
  uptime_seconds: 42,
  version: "0.0.0",
  goroutines: 10,
  memory_mb: 20,
  db: { chat: "healthy", sched: "healthy", open: 1, idle: 1 },
  llm: { calls_today: 0, cost_today_usd: 0, avg_cost_per_call: 0 },
  mcp_servers: [
    { name: "on-by-default-server", enabled: true },
    { name: "optional-server", enabled: false },
  ],
};

function mockFetch(body: unknown) {
  return vi.fn().mockImplementation(async () => ({
    ok: true,
    status: 200,
    json: async () => body,
  }));
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("HealthPanel MCP pills", () => {
  it("renders an optional (off-by-default) server as neutral, never danger", async () => {
    vi.stubGlobal("fetch", mockFetch(SUMMARY));
    render(<HealthPanel />);

    const optional = await screen.findByText("optional-server");
    const onByDefault = await screen.findByText("on-by-default-server");

    // The danger tone (which reads as "broken") must not be used for a
    // perfectly-normal optional server.
    expect(optional.className).not.toContain("--color-danger");
    // Optional → neutral; on-by-default → success. Tint distinguishes them.
    expect(optional.className).toContain("--color-border-strong"); // neutral tone
    expect(onByDefault.className).toContain("--color-success-strong"); // success tone

    // Tooltips explain what the tint means (it isn't a health probe).
    expect(optional.getAttribute("title")).toMatch(/optional/i);
    expect(onByDefault.getAttribute("title")).toMatch(/default/i);
  });

  it("never renders any MCP pill with the danger tone", async () => {
    vi.stubGlobal("fetch", mockFetch(SUMMARY));
    render(<HealthPanel />);
    await screen.findByText("optional-server");
    for (const name of ["on-by-default-server", "optional-server"]) {
      expect(screen.getByText(name).className).not.toContain("--color-danger");
    }
  });
});
