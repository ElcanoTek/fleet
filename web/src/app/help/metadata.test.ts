import { afterEach, describe, expect, it, vi } from "vitest";

const getServerBranding = vi.fn();
vi.mock("@/app/lib/serverBranding", () => ({ getServerBranding }));

const { guidePageTitle } = await import("./metadata");

afterEach(() => getServerBranding.mockReset());

// The root layout titles every tab with the BUNDLE's app name, and a page-level
// title replaces that string outright (there is no title.template). A bare
// "Guides" would therefore make /help the one surface whose tab drops a
// white-labeled deployment's name — so these two cases are the contract.
describe("guidePageTitle", () => {
  it("keeps the deployment's name in the tab", async () => {
    getServerBranding.mockResolvedValue({ appName: "Acme Intelligence" });
    expect(await guidePageTitle("Chat guide")).toBe("Chat guide — Acme Intelligence");
  });

  it("falls back to the page title alone rather than a dangling dash", async () => {
    getServerBranding.mockResolvedValue({ appName: "   " });
    expect(await guidePageTitle("Guides")).toBe("Guides");
  });
});
