import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { AccountMenu } from "./AccountMenu";

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
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    const button = screen.getByRole("button", { name: "Account menu" });
    expect(button).toHaveTextContent("sam@elcanotek.com");
    fireEvent.click(button);
    expect(screen.getByRole("menu", { name: "Account" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Theme" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Sign out" })).toBeInTheDocument();
  });

  it("invokes onSignOut from the menu", () => {
    const onSignOut = vi.fn();
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={onSignOut} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledTimes(1);
  });

  it("drives the theme via the System/Light/Dark segmented control", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("button", { name: "dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("dark");
    fireEvent.click(screen.getByRole("button", { name: "light" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("light");
  });

  it("System clears the stored preference and follows the OS", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("button", { name: "dark" }));
    expect(window.localStorage.getItem("chat-theme-preference")).toBe("dark");
    const systemButton = screen.getByRole("button", { name: "system" });
    fireEvent.click(systemButton);
    expect(window.localStorage.getItem("chat-theme-preference")).toBeNull();
    expect(systemButton).toHaveAttribute("aria-pressed", "true");
    // jsdom has no matchMedia by default → the hook's documented fallback is
    // dark; the load-bearing assertions are the cleared key + active segment.
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("defaults to System with no stored preference", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    expect(screen.getByRole("button", { name: "system" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("button", { name: "dark" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
  });

  it("closes on Escape", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    const menu = screen.getByRole("menu", { name: "Account" });
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(screen.queryByRole("menu", { name: "Account" })).toBeNull();
  });

  it("navigates to Settings from the menu; Connections/Skills/Admin are not menu items", () => {
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
      render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
      fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
      // Connections, Skills, and Admin live in the settings area's own
      // sub-nav now (Admin only for admins) — the menu must not offer them.
      // (Exact-name queries: the Settings item's accessible name includes its
      // "Connections, skills & workspace settings" subtext.)
      expect(screen.queryByRole("menuitem", { name: "Connections" })).toBeNull();
      expect(screen.queryByRole("menuitem", { name: "Skills" })).toBeNull();
      expect(screen.queryByRole("menuitem", { name: "Admin" })).toBeNull();
      fireEvent.click(screen.getByRole("menuitem", { name: /Settings/ }));
      expect(assign).toHaveBeenCalledWith("/settings");
    } finally {
      Object.defineProperty(window, "location", { value: original, configurable: true, writable: true });
    }
  });

  it("renders the Settings item's subtext line", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    const item = screen.getByRole("menuitem", { name: /Settings/ });
    expect(item).toHaveTextContent("Connections, skills & workspace settings");
  });

  it("tints the anchor and the Settings item on the settings surface", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} current />);
    const button = screen.getByRole("button", { name: "Account menu" });
    expect(button.className).toContain("account-current");
    fireEvent.click(button);
    expect(screen.getByRole("menuitem", { name: /Settings/ }).className).toContain(
      "menu-item-current",
    );
  });
});
