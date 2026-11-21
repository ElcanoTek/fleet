import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, renderHook, screen } from "@testing-library/react";
import {
  NavRail,
  RAIL_DEFAULT_WIDTH,
  RAIL_MAX_WIDTH,
  RAIL_MIN_WIDTH,
  RAIL_WIDTH_STORAGE_KEY,
  clampRailWidth,
  useRailCollapse,
  type RailCollapse,
} from "./NavRail";
import { resetSidebarStoreForTests } from "@/app/shared/sidebar/store";

// The sidebar's frame (docs/SIDEBAR.md): one scroller, the brand row as it
// has always been, the icon strip, and an edge that drags to resize.

vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: "member", team_id: "", admin: false }),
}));

beforeEach(() => {
  resetSidebarStoreForTests();
  window.localStorage.clear();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify({ layout: null, workspace_default: null, patrols: [] }), { status: 200 })),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function rail(collapse: Partial<RailCollapse> = {}, extra: Partial<Parameters<typeof NavRail>[0]> = {}) {
  const full: RailCollapse = { collapsed: false, isNarrow: false, railReady: true, setCollapsed: () => {}, ...collapse };
  return render(
    <NavRail
      activeView="chat"
      brandName="Acme"
      sidebarOpen={false}
      setSidebarOpen={() => {}}
      collapse={full}
      account={{ email: "someone@example.com", onSignOut: () => {} }}
      actions={<button type="button">search and new</button>}
      {...extra}
    >
      <ul data-testid="the-lists">
        <li>Pinned</li>
        <li>Chats</li>
      </ul>
    </NavRail>,
  );
}

describe("NavRail — one sidebar that scrolls as a whole", () => {
  it("puts the top items and the page's lists in the one scroller, and nothing else scrolls", () => {
    const { container } = rail();
    const body = screen.getByTestId("rail-body");
    expect(body).toContainElement(screen.getByTestId("nav-to-patrols"));
    expect(body).toContainElement(screen.getByTestId("the-lists"));
    // Search and the buttons beside it, and the account menu, stay in reach outside it.
    expect(body).not.toContainElement(screen.getByRole("button", { name: "search and new" }));
    expect(body).not.toContainElement(screen.getByRole("button", { name: /account menu/i }));
    const scrollers = Array.from(container.querySelectorAll("aside *")).filter((el) =>
      /(^|\s)overflow-(y-)?(auto|scroll)(\s|$)/.test(el.getAttribute("class") ?? ""),
    );
    expect(scrollers).toEqual([body]);
  });
});

describe("NavRail — the brand row is the one the live sidebar has", () => {
  it("draws the logo, the eyebrow and the name, and the logo is not a button", () => {
    rail();
    const logo = screen.getByAltText("Acme");
    expect(logo.tagName).toBe("IMG");
    expect(logo).toHaveAttribute("width", "28");
    expect(logo).toHaveAttribute("height", "28");
    expect(logo.closest("button")).toBeNull();
    expect(screen.getByText("Internal")).toBeInTheDocument();
    expect(screen.getByText("Acme")).toBeInTheDocument();
  });

  it("takes a deployment's own eyebrow", () => {
    rail({}, { eyebrow: "Staging" });
    expect(screen.getByText("Staging")).toBeInTheDocument();
    expect(screen.queryByText("Internal")).toBeNull();
  });

  it("has the gear (Customize) and the collapse toggle at the row's end", () => {
    const setCollapsed = vi.fn();
    rail({ setCollapsed });
    expect(screen.getByTestId("customize-sidebar-button")).toHaveAccessibleName("Customize sidebar");
    const toggle = screen.getByTestId("rail-collapse");
    expect(toggle).toHaveAccessibleName("Collapse sidebar");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(toggle);
    expect(setCollapsed).toHaveBeenCalledWith(true);
  });
});

describe("NavRail — the icon strip", () => {
  it("keeps the logo and an expand button under it", () => {
    const setCollapsed = vi.fn();
    rail({ collapsed: true, setCollapsed });
    expect(screen.getByAltText("Acme")).toBeInTheDocument();
    const toggle = screen.getByTestId("rail-expand");
    expect(toggle).toHaveAccessibleName("Expand sidebar");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByTestId("rail-collapse")).toBeNull();
    fireEvent.click(toggle);
    expect(setCollapsed).toHaveBeenCalledWith(false);
  });

  it("holds only the + and the top items: the gear and the divider are not drawn from sm up", () => {
    rail({ collapsed: true });
    // Collapsed is also what a phone reports (the drawer shows everything),
    // so each of these is hidden by width, not removed.
    expect(screen.getByTestId("customize-sidebar-button").className).toContain("sm:hidden");
    const divider = screen.getByTestId("the-lists").previousElementSibling as HTMLElement;
    expect(divider.className).toContain("sm:hidden");
    expect(screen.getByTestId("nav-to-patrols")).toBeInTheDocument();
    expect(screen.getByTestId("nav-to-orchestrator")).toBeInTheDocument();
  });

  it("names each icon for a screen reader", () => {
    rail({ collapsed: true });
    expect(screen.getByTestId("nav-to-patrols")).toHaveAccessibleName(/Patrols/);
    expect(screen.getByTestId("nav-to-orchestrator")).toHaveAccessibleName(/Background tasks/);
  });
});

describe("NavRail — drag the edge to resize", () => {
  const withWidth = (over: Partial<RailCollapse> = {}) => {
    const setWidth = vi.fn();
    rail({ width: null, setWidth, ...over });
    return setWidth;
  };

  it("is a focusable separator that says its width and its limits", () => {
    withWidth({ width: 360 });
    const handle = screen.getByTestId("rail-resize");
    expect(handle).toHaveAttribute("role", "separator");
    expect(handle).toHaveAttribute("aria-orientation", "vertical");
    expect(handle).toHaveAccessibleName("Resize sidebar");
    expect(handle).toHaveAttribute("aria-valuenow", "360");
    expect(handle).toHaveAttribute("aria-valuemin", String(RAIL_MIN_WIDTH));
    expect(handle).toHaveAttribute("aria-valuemax", String(RAIL_MAX_WIDTH));
    expect(handle).toHaveAttribute("tabindex", "0");
  });

  it("reports the default width until the person has dragged it", () => {
    withWidth();
    expect(screen.getByTestId("rail-resize")).toHaveAttribute("aria-valuenow", String(RAIL_DEFAULT_WIDTH));
  });

  it("is not offered in the icon strip, over the page as an overlay, or where the page cannot resize it", () => {
    const strip = rail({ collapsed: true, width: null, setWidth: () => {} });
    expect(screen.queryByTestId("rail-resize")).toBeNull();
    strip.unmount();
    const overlay = rail({ isNarrow: true, width: null, setWidth: () => {} });
    expect(screen.queryByTestId("rail-resize")).toBeNull();
    overlay.unmount();
    rail();
    expect(screen.queryByTestId("rail-resize")).toBeNull();
  });

  it("follows the pointer from the sidebar's own left edge while dragging", () => {
    const setWidth = withWidth();
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({ left: 20, width: 300 } as DOMRect);
    const handle = screen.getByTestId("rail-resize");
    // Moving without a press does nothing.
    fireEvent.pointerMove(handle, { clientX: 400 });
    expect(setWidth).not.toHaveBeenCalled();
    fireEvent.pointerDown(handle, { button: 0, pointerId: 1 });
    expect(handle).toHaveAttribute("data-dragging", "true");
    fireEvent.pointerMove(handle, { clientX: 420, pointerId: 1 });
    expect(setWidth).toHaveBeenLastCalledWith(400);
    fireEvent.pointerUp(handle, { pointerId: 1 });
    expect(handle).not.toHaveAttribute("data-dragging");
    fireEvent.pointerMove(handle, { clientX: 500, pointerId: 1 });
    expect(setWidth).toHaveBeenCalledTimes(1);
  });

  it("ignores a press that is not the main button", () => {
    const setWidth = withWidth();
    const handle = screen.getByTestId("rail-resize");
    fireEvent.pointerDown(handle, { button: 2, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 420, pointerId: 1 });
    expect(setWidth).not.toHaveBeenCalled();
  });

  it("resizes from the keyboard: arrows step, Home and End go to the limits, Enter resets", () => {
    const setWidth = withWidth({ width: 400 });
    // Steps are from the width the person set, never from the one being
    // drawn while the sidebar eases to it.
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({ left: 0, width: 371 } as DOMRect);
    const handle = screen.getByTestId("rail-resize");
    fireEvent.keyDown(handle, { key: "ArrowRight" });
    expect(setWidth).toHaveBeenLastCalledWith(416);
    fireEvent.keyDown(handle, { key: "ArrowLeft" });
    expect(setWidth).toHaveBeenLastCalledWith(384);
    fireEvent.keyDown(handle, { key: "Home" });
    expect(setWidth).toHaveBeenLastCalledWith(RAIL_MIN_WIDTH);
    fireEvent.keyDown(handle, { key: "End" });
    expect(setWidth).toHaveBeenLastCalledWith(RAIL_MAX_WIDTH);
    fireEvent.keyDown(handle, { key: "Enter" });
    expect(setWidth).toHaveBeenLastCalledWith(null);
    // Other keys are the page's.
    fireEvent.keyDown(handle, { key: "a" });
    expect(setWidth).toHaveBeenCalledTimes(5);
  });

  it("steps from the default width until the person has set one", () => {
    const setWidth = withWidth();
    fireEvent.keyDown(screen.getByTestId("rail-resize"), { key: "ArrowRight" });
    expect(setWidth).toHaveBeenLastCalledWith(RAIL_DEFAULT_WIDTH + 16);
  });

  it("a double-click puts the width back to the default", () => {
    const setWidth = withWidth({ width: 420 });
    fireEvent.doubleClick(screen.getByTestId("rail-resize"));
    expect(setWidth).toHaveBeenCalledWith(null);
  });
});

describe("useRailCollapse — the width is this screen's, and is remembered", () => {
  const stubViewport = (narrow: boolean) =>
    vi.stubGlobal(
      "matchMedia",
      vi.fn(() => ({ matches: narrow, addEventListener: () => {}, removeEventListener: () => {} })),
    );

  it("keeps a width inside its limits", () => {
    expect(clampRailWidth(100)).toBe(RAIL_MIN_WIDTH);
    expect(clampRailWidth(9999)).toBe(RAIL_MAX_WIDTH);
    expect(clampRailWidth(333.4)).toBe(333);
  });

  it("starts at no width of its own, takes one, remembers it, and gives it back", () => {
    stubViewport(false);
    const first = renderHook(() => useRailCollapse());
    expect(first.result.current.width).toBeNull();
    act(() => first.result.current.setWidth?.(1000));
    expect(first.result.current.width).toBe(RAIL_MAX_WIDTH);
    act(() => first.result.current.setWidth?.(352.6));
    expect(first.result.current.width).toBe(353);
    expect(window.localStorage.getItem(RAIL_WIDTH_STORAGE_KEY)).toBe("353");
    first.unmount();

    // The next page (or a reload) opens at the same width.
    const second = renderHook(() => useRailCollapse());
    expect(second.result.current.width).toBe(353);
    act(() => second.result.current.setWidth?.(null));
    expect(second.result.current.width).toBeNull();
    second.unmount();
    expect(renderHook(() => useRailCollapse()).result.current.width).toBeNull();
  });

  it("ignores a stored width that is not a number", () => {
    stubViewport(false);
    window.localStorage.setItem(RAIL_WIDTH_STORAGE_KEY, "wide");
    expect(renderHook(() => useRailCollapse()).result.current.width).toBeNull();
  });
});
