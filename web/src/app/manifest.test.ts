import { afterEach, describe, expect, it, vi } from "vitest";

afterEach(() => {
  delete process.env.NEXT_PUBLIC_APP_NAME;
  vi.resetModules();
});

describe("web app manifest", () => {
  it("uses the neutral Fleet name and complete install icon set by default", async () => {
    delete process.env.NEXT_PUBLIC_APP_NAME;
    const { default: manifest } = await import("./manifest");

    const value = manifest();
    expect(value.name).toBe("Fleet");
    expect(value.short_name).toBe("Fleet");
    expect(value.icons).toEqual([
      expect.objectContaining({ src: "/app-icons/icon-192.png", purpose: "any" }),
      expect.objectContaining({ src: "/app-icons/icon-512.png", purpose: "any" }),
      expect.objectContaining({ src: "/app-icons/maskable-icon-512.png", purpose: "maskable" }),
    ]);
  });

  it("uses the deployment's white-label app name", async () => {
    process.env.NEXT_PUBLIC_APP_NAME = "Example Workspace";
    const { default: manifest } = await import("./manifest");

    expect(manifest()).toEqual(expect.objectContaining({ name: "Example Workspace", short_name: "Example Workspace" }));
  });
});
