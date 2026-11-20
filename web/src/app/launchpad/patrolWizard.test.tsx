import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PatrolWizardHost, openPatrolWizard, resetPatrolWizardForTests, usePatrolWizardOpen } from "./patrolWizard";

// The patrol wizard as something any part of the page can open
// (docs/PATROL-LAUNCHPAD.md): the + menu, the Patrols page, a ?launchpad=1
// link. One host per page mounts it.

vi.mock("./PatrolLaunchpad", () => ({
  default: ({ onClose }: { onClose: () => void }) => (
    <div role="dialog" aria-label="Patrol Launchpad">
      <button type="button" onClick={onClose}>
        Close wizard
      </button>
    </div>
  ),
}));

const refreshPatrols = vi.hoisted(() => vi.fn(() => Promise.resolve([])));
vi.mock("@/app/shared/sidebar/targets", async (original) => ({
  ...(await original<typeof import("@/app/shared/sidebar/targets")>()),
  refreshPatrols,
}));

function OpenState() {
  return <p data-testid="open-state">{usePatrolWizardOpen() ? "open" : "closed"}</p>;
}

beforeEach(() => {
  resetPatrolWizardForTests();
  refreshPatrols.mockClear();
  window.history.replaceState(null, "", "/chat");
});
afterEach(() => {
  cleanup();
  resetPatrolWizardForTests();
});

describe("the patrol wizard, opened from anywhere on the page", () => {
  it("is not mounted until something asks for it", () => {
    render(<PatrolWizardHost />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens once however many times it is asked, and tells the page it is open", async () => {
    const onOpen = vi.fn();
    render(
      <>
        <PatrolWizardHost onOpen={onOpen} />
        <OpenState />
      </>,
    );
    act(() => {
      openPatrolWizard();
      openPatrolWizard();
    });
    expect(await screen.findByRole("dialog", { name: "Patrol Launchpad" })).toBeInTheDocument();
    expect(screen.getAllByRole("dialog")).toHaveLength(1);
    expect(screen.getByTestId("open-state")).toHaveTextContent("open");
    // The sidebar closes its phone drawer underneath, once.
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("closing it reads the patrols again, for every part of the page that lists them", async () => {
    render(
      <>
        <PatrolWizardHost />
        <OpenState />
      </>,
    );
    act(() => openPatrolWizard());
    fireEvent.click(await screen.findByRole("button", { name: "Close wizard" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("open-state")).toHaveTextContent("closed");
    expect(refreshPatrols).toHaveBeenCalledTimes(1);
  });

  it("hands the focus back to what opened it", async () => {
    // jsdom lays nothing out; report the opener as on screen.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
      width: 10, height: 10, top: 0, left: 0, right: 10, bottom: 10,
    } as DOMRect);
    render(
      <>
        <button type="button" data-testid="opener" onClick={(e) => openPatrolWizard(e.currentTarget)}>
          New patrol
        </button>
        <PatrolWizardHost />
      </>,
    );
    fireEvent.click(screen.getByTestId("opener"));
    const close = await screen.findByRole("button", { name: "Close wizard" });
    close.focus();
    fireEvent.click(close);
    await waitFor(() => expect(screen.getByTestId("opener")).toHaveFocus());
    vi.restoreAllMocks();
  });

  it("a ?launchpad=1 link opens it once and leaves the address clean", async () => {
    window.history.replaceState(null, "", "/patrols?launchpad=1&lang=es#top");
    render(<PatrolWizardHost />);
    expect(await screen.findByRole("dialog", { name: "Patrol Launchpad" })).toBeInTheDocument();
    // A reload or a shared URL must not reopen it for ever.
    expect(window.location.pathname + window.location.search + window.location.hash).toBe("/patrols?lang=es#top");
  });

  it("any other value of the parameter is not the link", () => {
    window.history.replaceState(null, "", "/chat?launchpad=0");
    render(<PatrolWizardHost />);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(window.location.search).toBe("?launchpad=0");
  });
});
