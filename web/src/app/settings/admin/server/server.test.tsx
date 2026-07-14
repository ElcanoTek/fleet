import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import AdminServerPage from "./page";

vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: vi.fn() }) }));
vi.mock("../../useIsAdmin", () => ({ useIsAdmin: () => "admin" }));

const STATS = {
  available: true,
  sampled_at: "2026-07-14T12:00:00Z",
  hostname: "fleet-box",
  platform: "linux/amd64",
  uptime_seconds: 90061,
  cpu: { available: true, cores: 8, usage_percent: 37.5, load_1: 0.5, load_5: 0.4, load_15: 0.3 },
  memory: { available: true, total_bytes: 8 * 1024 ** 3, used_bytes: 3 * 1024 ** 3, available_bytes: 5 * 1024 ** 3, swap_total_bytes: 0, swap_used_bytes: 0 },
  disk: { available: true, path: "/", total_bytes: 100 * 1024 ** 3, used_bytes: 25 * 1024 ** 3, available_bytes: 75 * 1024 ** 3, usage_percent: 25 },
  network: { available: true, interfaces: 1, received_bytes: 10 * 1024 ** 3, transmitted_bytes: 2 * 1024 ** 3, receive_bytes_per_second: 1536, transmit_bytes_per_second: 512 },
  warnings: [],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("AdminServerPage", () => {
  it("renders lightweight CPU, memory, disk, and network host metrics", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => STATS }));
    render(<AdminServerPage />);

    const panel = await screen.findByTestId("server-stats-panel");
    expect(panel).toHaveTextContent("37.5%");
    expect(panel).toHaveTextContent("8 logical cores");
    expect(panel).toHaveTextContent("1d 1h");
    expect(panel).toHaveTextContent("fleet-box");
    expect(panel).toHaveTextContent("5.00 GB available");
    expect(panel).toHaveTextContent("75.0 GB available");
    expect(panel).toHaveTextContent("1.50 KB/s");
    expect(screen.getByLabelText("Memory: 37.5% used")).toBeInTheDocument();
    expect(screen.getByLabelText("Root disk: 25.0% used")).toBeInTheDocument();
  });

  it("manually refreshes and surfaces partial-collection warnings", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ ...STATS, warnings: ["network statistics unavailable"] }),
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<AdminServerPage />);
    await screen.findByTestId("server-stats-panel");
    expect(screen.getByTestId("server-stats-warnings")).toHaveTextContent("network statistics unavailable");
    fireEvent.click(screen.getByRole("button", { name: "Refresh server stats" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
  });
});
