import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import AdminDoctorPage from "./page";

vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: vi.fn() }) }));
vi.mock("../../useIsAdmin", () => ({ useIsAdmin: () => "admin" }));

const HEALTHY = {
  generated_at: "2026-07-22T12:00:00Z",
  duration_ms: 1234,
  deep: false,
  healthy: true,
  summary: { ok: 9, warn: 0, fail: 0, skip: 2 },
  checks: [
    { name: "chat database", status: "ok", detail: "reachable via the server pool" },
    { name: "sandbox image", status: "ok", detail: "localhost/fleet-sandbox:latest present (deep run skipped)" },
    { name: "unit caddy", status: "skip", detail: "caddy.service not installed (optional tier)" },
  ],
};

const UNHEALTHY = {
  ...HEALTHY,
  healthy: false,
  summary: { ok: 8, warn: 1, fail: 1, skip: 1 },
  checks: [
    ...HEALTHY.checks,
    {
      name: "subuid range",
      status: "fail",
      detail: "/etc/subuid has no range for fleet — rootless podman cannot map the userns",
      fix: "run on the box: sudo fleet doctor",
    },
    { name: "disk: data dir", status: "warn", detail: "/var/lib/fleet: 87.0% used, 6.1 GiB free", fix: "consider: sudo fleet cleanup" },
  ],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("AdminDoctorPage", () => {
  it("renders the quick report with per-check status and a healthy badge", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => HEALTHY }));
    render(<AdminDoctorPage />);

    const panel = await screen.findByTestId("doctor-panel");
    expect(panel).toHaveTextContent("chat database");
    expect(panel).toHaveTextContent("reachable via the server pool");
    expect(panel).toHaveTextContent("deep run skipped");
    expect(screen.getByText("healthy")).toBeInTheDocument();
    expect(screen.queryByTestId("doctor-attention")).toBeNull();
    // The page-load run must be the QUICK one — never launch a container on mount.
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/admin/doctor");
  });

  it("surfaces failing checks with their on-box fix commands", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => UNHEALTHY }));
    render(<AdminDoctorPage />);

    const panel = await screen.findByTestId("doctor-panel");
    expect(screen.getByTestId("doctor-attention")).toHaveTextContent("1 check(s) failing");
    expect(panel).toHaveTextContent("subuid range");
    expect(panel).toHaveTextContent("run on the box: sudo fleet doctor");
    expect(panel).toHaveTextContent("consider: sudo fleet cleanup");
    expect(screen.getByText("1 failing")).toBeInTheDocument();
  });

  it("requests deep mode only via the explicit button", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => HEALTHY })
      .mockResolvedValueOnce({ ok: true, status: 200, json: async () => ({ ...HEALTHY, deep: true }) });
    vi.stubGlobal("fetch", fetchMock);
    render(<AdminDoctorPage />);
    await screen.findByTestId("doctor-panel");

    fireEvent.click(screen.getByTestId("doctor-run-deep"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(fetchMock.mock.calls[1][0]).toBe("/api/admin/doctor?deep=1");
    await waitFor(() => expect(screen.getByText(/^deep run ·/)).toBeInTheDocument());
  });

  it("shows the error banner when the report cannot load", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 502, json: async () => ({}) }));
    render(<AdminDoctorPage />);
    expect(await screen.findByTestId("doctor-error")).toHaveTextContent("502");
  });
});
