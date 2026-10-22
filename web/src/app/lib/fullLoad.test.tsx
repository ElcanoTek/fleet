import { describe, expect, it, vi, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { needsFullLoad } from "./fullLoad";
import NoAccessPage from "@/app/no-access/page";

vi.mock("@/app/no-access/sign-out-form", () => ({ SignOutForm: () => null }));

describe("needsFullLoad", () => {
  it("is true into or out of a server-decided route, false between shell routes", () => {
    expect(needsFullLoad("/no-access", "/chat")).toBe(true);
    expect(needsFullLoad("/settings", "/login")).toBe(true);
    expect(needsFullLoad("/chat", "/shared/abc")).toBe(true);
    expect(needsFullLoad("/settings", "/chat")).toBe(false);
    expect(needsFullLoad("/chat", "/orchestrator")).toBe(false);
  });
});

describe("/no-access Try again", () => {
  afterEach(() => window.history.replaceState(null, "", "/"));

  it("leaves the page with a real navigation so the server re-runs the membership gate", async () => {
    window.history.replaceState(null, "", "/no-access");
    const push = vi.spyOn(window.history, "pushState");
    render(<NoAccessPage />);
    const link = screen.getByRole("link", { name: "Try again" });
    // window sees the click after React's root listener (where <Link> runs):
    // the router must have left it to the browser (not prevented it).
    let prevented: boolean | undefined;
    const onClick = (e: MouseEvent) => {
      prevented = e.defaultPrevented;
      e.preventDefault(); // jsdom cannot navigate; record the router's decision only
    };
    window.addEventListener("click", onClick);
    await userEvent.click(link);
    window.removeEventListener("click", onClick);
    expect(prevented).toBe(false);
    expect(push).not.toHaveBeenCalled();
    expect(link.getAttribute("href")).toBe("/chat");
  });

  it("a shell route still soft-navigates (control)", async () => {
    window.history.replaceState(null, "", "/settings");
    const { Link } = await import("./Link");
    render(<Link href="/chat">Chat</Link>);
    let prevented: boolean | undefined;
    const onClick = (e: MouseEvent) => {
      prevented = e.defaultPrevented;
      e.preventDefault();
    };
    window.addEventListener("click", onClick);
    await userEvent.click(screen.getByRole("link", { name: "Chat" }));
    window.removeEventListener("click", onClick);
    expect(prevented).toBe(true);
    expect(window.location.pathname).toBe("/chat");
  });
});
