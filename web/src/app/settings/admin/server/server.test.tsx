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

const STORAGE = {
  disk_total_bytes: 100 * 1024 ** 3,
  disk_available_bytes: 75 * 1024 ** 3,
  uploads: { path: "./data/attachments/uploads", bytes: 2 * 1024 ** 3, files: 12 },
  temp_uploads: { path: "./data/temp_uploads", bytes: 512 * 1024 ** 2, files: 3 },
  workspaces: { path: "workspace", bytes: 4 * 1024 ** 3, files: 200 },
  conversations_total: 42,
  conversations_pinned: 5,
  conversations_protected: 9,
  reclaimable_conversations: 7,
  default_days: 30,
  largest_workspaces: [
    { conversation_id: "0b6f2c1e-1111-2222-3333-444455556666", bytes: 3 * 1024 ** 3, title: "big analysis", user_email: "u@x.com", pinned: false, orphaned: false },
  ],
};

// Serve each endpoint its own payload — the page fetches host stats AND
// the storage accounting on mount.
const routedFetch = (stats: unknown = STATS, storage: unknown = STORAGE) =>
  vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    const body = url.includes("/api/admin/storage") ? storage : stats;
    return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
  });

describe("AdminServerPage", () => {
  it("renders lightweight CPU, memory, disk, and network host metrics", async () => {
    vi.stubGlobal("fetch", routedFetch());
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
    const fetchMock = routedFetch({ ...STATS, warnings: ["network statistics unavailable"] });
    vi.stubGlobal("fetch", fetchMock);
    render(<AdminServerPage />);
    await screen.findByTestId("server-stats-panel");
    expect(screen.getByTestId("server-stats-warnings")).toHaveTextContent("network statistics unavailable");
    const before = fetchMock.mock.calls.length;
    fireEvent.click(screen.getByRole("button", { name: "Refresh server stats" }));
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(before));
  });

  it("renders the storage panel with tree totals and the cleanup preview", async () => {
    vi.stubGlobal("fetch", routedFetch());
    render(<AdminServerPage />);

    const panel = await screen.findByTestId("storage-panel");
    expect(panel).toHaveTextContent("Attachment uploads");
    expect(panel).toHaveTextContent("2.00 GB");
    expect(panel).toHaveTextContent("big analysis");
    expect(panel).toHaveTextContent("u@x.com");
    // Cleanup preview names the reclaimable count from the API.
    expect(panel).toHaveTextContent("would remove 7 conversations");
    expect(screen.getByTestId("storage-cleanup-run")).toBeEnabled();
  });

  it("degrades to an error banner when the storage endpoint is missing", async () => {
    // Older server: /api/admin/storage returns a non-storage payload.
    vi.stubGlobal("fetch", routedFetch(STATS, { error: "not found" }));
    render(<AdminServerPage />);
    await screen.findByTestId("server-stats-panel");
    expect(await screen.findByTestId("storage-error")).toHaveTextContent("storage stats unavailable on this server");
  });
});
