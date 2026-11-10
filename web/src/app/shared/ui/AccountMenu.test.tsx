import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { AccountMenu } from "./AccountMenu";

let mockAdminState: "unknown" | "admin" | "member" = "admin";
vi.mock("@/app/settings/useIsAdmin", () => ({
  useIsAdmin: () => mockAdminState,
}));

beforeEach(() => {
  mockAdminState = "admin";
});

afterEach(() => {
  cleanup();
  try {
    window.localStorage.clear();
  } catch {
    /* ignore */
  }
});

describe("AccountMenu", () => {
  it("shows the account button with the email and opens the menu", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    const button = screen.getByRole("button", { name: /account menu/i });
    expect(button).toHaveTextContent("sam@elcanotek.com");
    // Label in Name (WCAG 2.5.3): the visible email is part of the name.
    expect(button).toHaveAccessibleName("sam@elcanotek.com, account menu");
    fireEvent.click(button);
    expect(screen.getByRole("menu", { name: "Account" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Theme" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Sign out" })).toBeInTheDocument();
  });

  it("groups items under Workspace, Model Context & Tools, and Administration (for admins)", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} guidesCurrent />);
    const button = screen.getByRole("button", { name: /account menu/i });
    expect(button.className).toContain("account-current");
    fireEvent.click(button);
    const menu = screen.getByRole("menu", { name: "Account" });
    expect(within(menu).getByText("Workspace")).toBeInTheDocument();
    expect(within(menu).getByText("Model Context & Tools")).toBeInTheDocument();
    expect(within(menu).getByText("Administration")).toBeInTheDocument();
    expect(screen.getByTestId("nav-to-settings")).toBeInTheDocument();
    expect(screen.getByTestId("nav-to-help")).toHaveClass("menu-item-current");
    expect(screen.getByTestId("nav-to-help")).toHaveAttribute("aria-current", "page");
    expect(screen.getByTestId("nav-to-context-settings")).toBeInTheDocument();
    expect(screen.getByTestId("nav-to-admin-settings")).toBeInTheDocument();
    expect(screen.getByTestId("nav-to-analytics")).not.toHaveAttribute("aria-current");
    expect(screen.getByTestId("nav-to-settings")).not.toHaveAttribute("aria-current");
  });

  it("hides Administration (Admin Settings & Analytics) for non-admin members", () => {
    mockAdminState = "member";
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    const menu = screen.getByRole("menu", { name: "Account" });
    expect(within(menu).getByText("Workspace")).toBeInTheDocument();
    expect(within(menu).getByText("Model Context & Tools")).toBeInTheDocument();
    expect(within(menu).queryByText("Administration")).toBeNull();
    expect(screen.queryByTestId("nav-to-admin-settings")).toBeNull();
    expect(screen.queryByTestId("nav-to-analytics")).toBeNull();
  });

  it("tints Analytics when analyticsCurrent is set", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} analyticsCurrent />);
    const button = screen.getByRole("button", { name: /account menu/i });
    expect(button.className).toContain("account-current");
    fireEvent.click(button);
    expect(screen.getByTestId("nav-to-analytics")).toHaveClass("menu-item-current");
    expect(screen.getByTestId("nav-to-analytics")).toHaveAttribute("aria-current", "page");
  });

  // Sign out asks first: the item opens a centred confirm (portalled out of
  // the rail, whose transform would otherwise pin the overlay to it) and only
  // the confirm's Sign out button signs out.
  function openSignOutConfirm(onSignOut: () => void) {
    const view = render(<AccountMenu email="sam@elcanotek.com" appName="Victoria" onSignOut={onSignOut} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    return { ...view, dialog: screen.getByRole("dialog", { name: "Sign out?" }) };
  }

  it("asks before signing out, in a dialog outside the rail", () => {
    const onSignOut = vi.fn();
    const { container, dialog } = openSignOutConfirm(onSignOut);
    expect(onSignOut).not.toHaveBeenCalled();
    expect(screen.queryByRole("menu", { name: "Account" })).toBeNull();
    expect(dialog).toHaveTextContent("every app that shares your sign-in");
    // Named as the deployment brands itself, not as the engine.
    expect(dialog).toHaveTextContent("This signs you out of Victoria and");
    expect(dialog).not.toHaveTextContent("Fleet");
    expect(container.contains(dialog)).toBe(false);
    expect(document.body.contains(dialog)).toBe(true);
    // Above the rail where it floats over the page (DialogShell's "rail").
    expect(dialog.parentElement?.className).toContain("z-[420]");
  });

  it("signs out once from the confirm's Sign out button", () => {
    const onSignOut = vi.fn();
    const { dialog } = openSignOutConfirm(onSignOut);
    fireEvent.click(within(dialog).getByRole("button", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: "Sign out?" })).toBeNull();
  });

  it("Cancel and Escape close the confirm without signing out", () => {
    const onSignOut = vi.fn();
    const { dialog } = openSignOutConfirm(onSignOut);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog", { name: "Sign out?" })).toBeNull();
    // Focus goes back to the account button, not the page.
    expect(document.activeElement).toBe(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog", { name: "Sign out?" })).toBeNull();
    expect(onSignOut).not.toHaveBeenCalled();
  });

  it("drives the theme via the System/Light/Dark segmented control", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("button", { name: "Dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("dark");
    fireEvent.click(screen.getByRole("button", { name: "Light" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("light");
  });

  it("System clears the stored preference and follows the OS", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("button", { name: "Dark" }));
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("dark");
    const systemButton = screen.getByRole("button", { name: "System" });
    fireEvent.click(systemButton);
    expect(window.localStorage.getItem("chat-theme-preference")).toBeNull();
    expect(systemButton).toHaveAttribute("aria-pressed", "true");
    // jsdom has no matchMedia by default → the hook's documented fallback is
    // dark; the load-bearing assertions are the cleared key + active segment.
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("defaults to System with no stored preference", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    expect(screen.getByRole("button", { name: "System" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "Dark" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("closes on Escape", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    const menu = screen.getByRole("menu", { name: "Account" });
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(screen.queryByRole("menu", { name: "Account" })).toBeNull();
  });

  it("navigates to Settings, Context & Connections, and Admin Settings from the menu", () => {
    // jsdom marks location.assign non-configurable; swap the whole location
    // object for a stub to observe navigations.
    const original = window.location;
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      value: { ...original, assign },
      configurable: true,
      writable: true,
    });
    try {
      render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
      fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
      fireEvent.click(screen.getByTestId("nav-to-settings"));
      expect(assign).toHaveBeenCalledWith("/settings");

      fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
      fireEvent.click(screen.getByTestId("nav-to-context-settings"));
      expect(assign).toHaveBeenCalledWith("/settings/connections");

      fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
      fireEvent.click(screen.getByTestId("nav-to-admin-settings"));
      expect(assign).toHaveBeenCalledWith("/settings/admin");
    } finally {
      Object.defineProperty(window, "location", { value: original, configurable: true, writable: true });
    }
  });

  it("renders the Settings item's subtext line", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    const item = screen.getByTestId("nav-to-settings");
    expect(item).toHaveTextContent("General preferences & team settings");
  });

  it("tints the anchor and the Settings item on the settings surface", () => {
    render(<AccountMenu email="sam@elcanotek.com" appName="Fleet" onSignOut={() => {}} current />);
    const button = screen.getByRole("button", { name: /account menu/i });
    expect(button.className).toContain("account-current");
    fireEvent.click(button);
    expect(screen.getByTestId("nav-to-settings").className).toContain(
      "menu-item-current",
    );
  });
});
