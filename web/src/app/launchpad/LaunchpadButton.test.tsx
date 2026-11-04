import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

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

// The rail's Patrols item: patrols live in Chat, so with none yet it opens the
// Launchpad wizard; with some it lists them (each opens its chat) beside "New
// patrol". A "new" dot shows until first opened; ?launchpad=1 opens the wizard.

function stubPatrols(patrols: unknown[]) {
  const fn = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ patrols }) });
  vi.stubGlobal("fetch", fn);
  return fn;
}

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/chat");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("LaunchpadButton", () => {
  it("opens the wizard when there are no patrols yet, and the dot goes away", async () => {
    stubPatrols([]);
    render(<LaunchpadButton />);
    expect(screen.getByTestId("launchpad-new-dot")).toBeTruthy();
    fireEvent.click(screen.getByTestId("launchpad-button"));
    expect(await screen.findByTestId("patrol-launchpad")).toBeTruthy();
    expect(screen.queryByTestId("launchpad-new-dot")).toBeNull();
    expect(window.localStorage.getItem(LAUNCHPAD_SEEN_ITEM)).toBe("1");
    fireEvent.click(screen.getByText("close"));
    expect(screen.queryByTestId("patrol-launchpad")).toBeNull();
  });

  it("lists existing patrols, each opening its chat, beside New patrol", async () => {
    stubPatrols([
      { id: "s1", conversation_id: "c1", title: "Deal sweep", conversation_title: "Deals chat", cron: "0 * * * *", status: "active" },
      { id: "s2", conversation_id: "c2", title: "Inbox triage", conversation_title: "Inbox triage", cron: "0 */2 * * *", status: "paused" },
    ]);
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    render(<LaunchpadButton />);
    fireEvent.click(screen.getByTestId("launchpad-button"));
    const menu = await screen.findByRole("menu", { name: "Patrols" });
    expect(menu.textContent).toContain("Each patrol is a chat");
    expect(screen.getByTestId("patrols-menu-item-s1").textContent).toMatch(/Deal sweep.*Deals chat · Every hour/);
    expect(screen.getByTestId("patrols-menu-item-s2").textContent).toMatch(/Inbox triage.*Every 2h · Paused/);
    expect(screen.queryByTestId("patrol-launchpad")).toBeNull();

    fireEvent.click(screen.getByTestId("patrols-menu-item-s1"));
    expect(assign).toHaveBeenCalledWith("/chat?c=c1");
  });

  it("opens the wizard from New patrol in the menu", async () => {
    stubPatrols([{ id: "s1", conversation_id: "c1", title: "Deal sweep", conversation_title: "x", cron: "0 * * * *", status: "active" }]);
    render(<LaunchpadButton />);
    fireEvent.click(screen.getByTestId("launchpad-button"));
    fireEvent.click(await screen.findByTestId("patrols-menu-new"));
    expect(await screen.findByTestId("patrol-launchpad")).toBeTruthy();
  });

  it("has no dot for someone who has opened it before", () => {
    window.localStorage.setItem(LAUNCHPAD_SEEN_ITEM, "1");
    stubPatrols([]);
    render(<LaunchpadButton />);
    expect(screen.queryByTestId("launchpad-new-dot")).toBeNull();
    expect(screen.getByRole("button", { name: /Patrols/ })).toBeTruthy();
  });

  it("opens the wizard from ?launchpad=1 and drops the parameter", async () => {
    stubPatrols([]);
    window.history.replaceState(null, "", "/settings/memories?launchpad=1&x=2#top");
    render(<LaunchpadButton />);
    expect(await screen.findByTestId("patrol-launchpad")).toBeTruthy();
    await waitFor(() =>
      expect(window.location.pathname + window.location.search + window.location.hash).toBe("/settings/memories?x=2#top"),
    );
  });
});
