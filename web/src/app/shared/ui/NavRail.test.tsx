import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { NavRail, type RailCollapse } from "./NavRail";

afterEach(() => cleanup());

// CrossViewNav renders router links; the rail itself needs no routing behavior for
// these assertions, so stub the two nav buttons out.
vi.mock("./CrossViewNav", () => ({
  NavToChat: () => <a href="/chat">Chat</a>,
  NavToOrchestrator: () => <a href="/orchestrator">Operations</a>,
  NavToHelp: () => <a href="/help">Guides</a>,
}));

const collapse: RailCollapse = {
  collapsed: false,
  isNarrow: false,
  railReady: true,
  setCollapsed: () => {},
};

function renderRail(brandLogoSrc?: string) {
  return render(
    <NavRail
      activeView="chat"
      brandName="Acme"
      brandLogoSrc={brandLogoSrc}
      sidebarOpen={false}
      setSidebarOpen={() => {}}
      collapse={collapse}
      account={{ email: "someone@example.com", onSignOut: () => {} }}
    />,
  );
}

describe("NavRail brand mark", () => {
  // Regression guard for the bundle-logo defect: next/image bypasses its
  // optimizer ONLY for a src path literally ending in ".svg". A bundle mark
  // arrives as "/api/brand/logo" with no extension, so without `unoptimized` it
  // was rewritten to "/_next/image?url=..." — and that endpoint rejects
  // image/svg+xml unless images.dangerouslyAllowSVG is set (it is not), so every
  // page rendered a broken mark for any bundle whose branding.logo was an SVG.
  it("serves an extensionless bundle logo as-is, not through the image optimizer", () => {
    renderRail("/api/brand/logo");
    const img = screen.getByAltText("Acme");
    expect(img.getAttribute("src")).toBe("/api/brand/logo");
    expect(img.getAttribute("src")).not.toContain("/_next/image");
  });

  it("serves fleet's own default mark as-is too", () => {
    renderRail();
    const img = screen.getByAltText("Acme");
    expect(img.getAttribute("src")).toBe("/logos/fleet-mark.svg");
    expect(img.getAttribute("src")).not.toContain("/_next/image");
  });

  it("does not emit a srcset — an unoptimized mark has no generated variants", () => {
    renderRail("/api/brand/logo");
    expect(screen.getByAltText("Acme").getAttribute("srcset")).toBeNull();
  });
});

describe("NavRail account menu", () => {
  // The Sign out confirm names the deployment as the brand row does
  // (branding.app_name), not as "Fleet".
  it("names the brand in the Sign out confirm", () => {
    renderRail();
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(screen.getByRole("dialog", { name: "Sign out?" })).toHaveTextContent(
      "This signs you out of Acme and of every app that shares your sign-in.",
    );
  });
});

describe("NavRail phone drawer", () => {
  // The closed drawer is invisible, so focus left inside it would fall to
  // <body>. Closing it hands focus to the button that opens it.
  it("returns focus to the visible sidebar button when it closes with focus inside", () => {
    // jsdom lays nothing out: every element reports no offsetParent, which the
    // hand-back reads as "hidden".
    const offsetParent = vi
      .spyOn(HTMLElement.prototype, "offsetParent", "get")
      .mockReturnValue(document.body);
    try {
      const rail = (open: boolean) => (
        <>
          <button type="button" data-sidebar-toggle="">Open sidebar</button>
          <NavRail
            activeView="chat"
            brandName="Acme"
            sidebarOpen={open}
            setSidebarOpen={() => {}}
            collapse={collapse}
            account={{ email: "someone@example.com", onSignOut: () => {} }}
          />
        </>
      );
      const { rerender } = render(rail(true));
      screen.getByRole("button", { name: "Close sidebar" }).focus();
      rerender(rail(false));
      expect(screen.getByRole("button", { name: "Open sidebar" })).toHaveFocus();
    } finally {
      offsetParent.mockRestore();
    }
  });

  it("leaves focus alone when the drawer closes with focus elsewhere", () => {
    const rail = (open: boolean) => (
      <>
        <button type="button" data-sidebar-toggle="">Open sidebar</button>
        <input aria-label="Message" />
        <NavRail
          activeView="chat"
          brandName="Acme"
          sidebarOpen={open}
          setSidebarOpen={() => {}}
          collapse={collapse}
          account={{ email: "someone@example.com", onSignOut: () => {} }}
        />
      </>
    );
    const { rerender } = render(rail(true));
    screen.getByLabelText("Message").focus();
    rerender(rail(false));
    expect(screen.getByLabelText("Message")).toHaveFocus();
  });
});
