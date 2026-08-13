import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, cleanup, act } from "@testing-library/react";
import { ServerClock } from "./ServerClock";

const config = vi.fn();

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: { config: (...args: unknown[]) => config(...args) },
}));

// The browser clock is pinned far away from the server's on purpose: every
// assertion below would still pass "by accident" if the component rendered local
// time, so the gap is what proves it is showing the SERVER's clock.
const BROWSER_NOW = new Date("2026-08-13T09:00:00.000Z");

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(BROWSER_NOW);
  config.mockReset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// settle lets the mocked config promise resolve and the first tick land.
// waitFor cannot be used here: it polls on real timers, which are faked.
async function settle() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

describe("ServerClock", () => {
  it("shows the server's wall clock, not the browser's", async () => {
    // Server is an hour and 23 minutes ahead of this browser, in New York.
    config.mockResolvedValue({
      timezone: "America/New_York",
      server_time: "2026-08-13T06:23:00-04:00", // 10:23Z
    });
    render(<ServerClock />);

    await settle();
    const clock = screen.getByTestId("server-clock");
    // 06:23 in New York — NOT 09:00 (the browser's UTC clock) and not 10:23
    // (the same instant read in UTC).
    expect(clock.textContent).toContain("6:23");
    expect(clock.textContent).not.toContain("9:00");
    expect(clock).toHaveAttribute("title", expect.stringContaining("America/New_York"));
  });

  it("ticks forward once a second without refetching", async () => {
    config.mockResolvedValue({
      timezone: "UTC",
      server_time: "2026-08-13T06:23:00+00:00",
    });
    render(<ServerClock />);
    await settle();
    expect(screen.getByTestId("server-clock").textContent).toContain("6:23:00");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(screen.getByTestId("server-clock").textContent).toContain("6:23:02");
    // Two seconds of ticking must not have cost two requests.
    expect(config).toHaveBeenCalledTimes(1);
  });

  it("still renders the right wall time when the runtime rejects the zone", async () => {
    // An unknown/misspelled zone makes Intl throw; the offset the server sent
    // with its timestamp is the fallback, which is why it travels with it.
    config.mockResolvedValue({
      timezone: "Mars/Olympus_Mons",
      server_time: "2026-08-13T06:23:00-04:00",
    });
    render(<ServerClock />);

    await settle();
    const text = screen.getByTestId("server-clock").textContent ?? "";
    expect(text).toContain("6:23");
    expect(text).toContain("UTC-04:00");
  });

  it("renders nothing rather than a guess when the server sends no time", async () => {
    config.mockResolvedValue({ timezone: "UTC" });
    render(<ServerClock />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    // A clock with no server reading would just be local time wearing a
    // "Server time" label — worse than absent.
    expect(screen.queryByTestId("server-clock")).toBeNull();
  });

  it("renders nothing when the config call fails", async () => {
    config.mockRejectedValue(new Error("nope"));
    render(<ServerClock />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(screen.queryByTestId("server-clock")).toBeNull();
  });

  it("names the task-scheduling zone when it differs from the server clock", async () => {
    config.mockResolvedValue({
      timezone: "UTC",
      default_task_timezone: "America/New_York",
      server_time: "2026-08-13T06:23:00+00:00",
    });
    render(<ServerClock />);
    await settle();
    // The zone a cron actually fires in is the trap worth surfacing.
    expect(screen.getByTestId("server-clock")).toHaveAttribute(
      "title",
      expect.stringContaining("America/New_York"),
    );
  });
});
