import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NavRail, type RailCollapse } from "./NavRail";

afterEach(() => cleanup());

// CrossViewNav renders router links; the rail itself needs no routing behavior for
// these assertions, so stub the two nav buttons out.
vi.mock("./CrossViewNav", () => ({
  NavToChat: () => <a href="/chat">Chat</a>,
  NavToOrchestrator: () => <a href="/orchestrator">Operations</a>,
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
