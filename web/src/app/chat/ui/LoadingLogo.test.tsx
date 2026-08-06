import { afterEach, describe, expect, it, vi } from "vitest";
import { render, cleanup } from "@testing-library/react";

// LoadingLogo renders fleet's orbital mark by default, but on a branded
// deployment (bundle declares branding.logo) it must show the CLIENT's mark —
// the vendor's animated logo in the client's chat is a branding leak.

const clientConfig = vi.hoisted(() => ({ logoUrl: "" }));
vi.mock("@/app/lib/useClientConfig", () => ({
  useClientConfig: () => ({
    branding: { app_name: "Test", logo_url: clientConfig.logoUrl },
    pills: [],
    loading: false,
  }),
}));

import { LoadingLogo } from "./LoadingLogo";

afterEach(() => {
  cleanup();
  clientConfig.logoUrl = "";
});

describe("LoadingLogo", () => {
  it("renders the fleet orbital mark when the bundle declares no logo", () => {
    const { container } = render(<LoadingLogo size={20} />);
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("svg[aria-label='Loading']")).not.toBeNull();
  });

  it("renders the bundle's brand mark when branding.logo_url is set", () => {
    clientConfig.logoUrl = "/api/brand/logo";
    const { container } = render(<LoadingLogo size={20} />);
    const img = container.querySelector("img");
    expect(img).not.toBeNull();
    expect(img?.getAttribute("src")).toBe("/api/brand/logo");
    // the spinner arc still rides the theme color around the brand mark
    expect(container.querySelector("circle")).not.toBeNull();
    // fleet's orbital star must not render
    expect(container.textContent).not.toContain("1135.96");
  });
});
