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

  it("omits Settings when no onSettings is provided (chat surface)", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    expect(screen.queryByRole("menuitem", { name: "Settings" })).toBeNull();
  });

  it("shows Settings when onSettings is provided (orchestrator surface) and invokes it", () => {
    const onSettings = vi.fn();
    render(<AccountMenu email="ops@elcanotek.com" onSignOut={() => {}} onSettings={onSettings} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Settings" }));
    expect(onSettings).toHaveBeenCalledTimes(1);
  });

  it("invokes onSignOut from the menu", () => {
    const onSignOut = vi.fn();
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={onSignOut} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(onSignOut).toHaveBeenCalledTimes(1);
  });

  it("drives the theme via the Light/Dark segmented control", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    fireEvent.click(screen.getByRole("button", { name: "dark" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    fireEvent.click(screen.getByRole("button", { name: "light" }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("closes on Escape", () => {
    render(<AccountMenu email="sam@elcanotek.com" onSignOut={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Account menu" }));
    const menu = screen.getByRole("menu", { name: "Account" });
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(screen.queryByRole("menu", { name: "Account" })).toBeNull();
  });
});
