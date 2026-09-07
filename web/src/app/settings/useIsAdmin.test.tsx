import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { resetAdminProbeForTests, retryAdminProbe, useIsAdmin } from "./useIsAdmin";
import { AdminGateFallback } from "./AdminGateFallback";

// useIsAdmin folds one admin-gated probe into a visibility state. The failure
// path is the one that matters here: a probe that settles WITHOUT an answer
// used to stay "unknown" forever, and every admin page renders null for
// "unknown" — a blank page with nothing to click. It now becomes
// "unavailable" (after one automatic re-probe), the pages render the
// AdminGateFallback notice for it, and Retry re-runs the probe.

beforeEach(() => {
  resetAdminProbeForTests({ retryDelayMs: 0 });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  resetAdminProbeForTests();
});

const answer = (status: number) => ({ ok: status < 400, status, json: async () => ({}) });

describe("useIsAdmin", () => {
  it("settles to unavailable after two failed probes, and recovers on retry", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValue(answer(200));
    vi.stubGlobal("fetch", fetchMock);

    const { result } = renderHook(() => useIsAdmin());
    expect(result.current).toBe("unknown");

    // First probe fails → one automatic re-probe → only then does it give
    // up, visibly.
    await waitFor(() => expect(result.current).toBe("unavailable"));
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Retry runs a fresh probe and every subscribed hook gets the answer.
    await act(async () => {
      await retryAdminProbe();
    });
    expect(result.current).toBe("admin");
  });

  it("caches an answer so later mounts do not re-probe", async () => {
    const fetchMock = vi.fn().mockResolvedValue(answer(403));
    vi.stubGlobal("fetch", fetchMock);
    const first = renderHook(() => useIsAdmin());
    await waitFor(() => expect(first.result.current).toBe("member"));
    first.unmount();
    const second = renderHook(() => useIsAdmin());
    expect(second.result.current).toBe("member");
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe("AdminGateFallback", () => {
  it("renders nothing while the probe is unresolved or for a member", () => {
    const { container, rerender } = render(<AdminGateFallback state="unknown" />);
    expect(container).toBeEmptyDOMElement();
    rerender(<AdminGateFallback state="member" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("says the check failed and re-probes on Retry", async () => {
    const fetchMock = vi.fn().mockResolvedValue(answer(200));
    vi.stubGlobal("fetch", fetchMock);
    render(<AdminGateFallback state="unavailable" />);
    const notice = screen.getByTestId("admin-gate-unavailable");
    expect(notice).toHaveTextContent("Couldn’t check your permissions");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith("/api/admin/settings", expect.anything()));
  });
});
