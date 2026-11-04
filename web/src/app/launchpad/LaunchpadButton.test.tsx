import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("./PatrolLaunchpad", () => ({
  default: ({ onClose }: { onClose: () => void }) => (
    <div data-testid="patrol-launchpad">
      <button type="button" onClick={onClose}>
        close
      </button>
    </div>
  ),
}));

import { LAUNCHPAD_SEEN_ITEM, LaunchpadButton } from "./LaunchpadButton";

// The Launchpad's top-bar entry point: a "new" dot until it is first opened,
// a click opens the wizard, and ?launchpad=1 opens it once and is consumed.

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/chat");
});

afterEach(() => {
  cleanup();
});

describe("LaunchpadButton", () => {
  it("shows the new dot until opened, then remembers", async () => {
    render(<LaunchpadButton />);
    expect(screen.getByTestId("launchpad-new-dot")).toBeTruthy();
    fireEvent.click(screen.getByTestId("launchpad-button"));
    expect(await screen.findByTestId("patrol-launchpad")).toBeTruthy();
    expect(screen.queryByTestId("launchpad-new-dot")).toBeNull();
    expect(window.localStorage.getItem(LAUNCHPAD_SEEN_ITEM)).toBe("1");
    fireEvent.click(screen.getByText("close"));
    expect(screen.queryByTestId("patrol-launchpad")).toBeNull();
  });

  it("has no dot for someone who has opened it before", () => {
    window.localStorage.setItem(LAUNCHPAD_SEEN_ITEM, "1");
    render(<LaunchpadButton />);
    expect(screen.queryByTestId("launchpad-new-dot")).toBeNull();
    expect(screen.getByRole("button", { name: /Patrol Launchpad/ })).toBeTruthy();
  });

  it("opens from ?launchpad=1 and drops the parameter", async () => {
    window.history.replaceState(null, "", "/settings/memories?launchpad=1&x=2#top");
    render(<LaunchpadButton />);
    expect(await screen.findByTestId("patrol-launchpad")).toBeTruthy();
    expect(window.location.pathname + window.location.search + window.location.hash).toBe("/settings/memories?x=2#top");
  });
});
