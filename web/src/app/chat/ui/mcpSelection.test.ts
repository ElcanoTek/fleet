import { describe, expect, it } from "vitest";
import { enabledOptionalMcpServerNames } from "./mcpSelection";

describe("enabledOptionalMcpServerNames", () => {
  it("persists selected optional connectors but never locked always-on rows", () => {
    expect(
      enabledOptionalMcpServerNames([
        { name: "email", enabled: true, always_on: true },
        { name: "broken", enabled: false, always_on: true },
        { name: "gamma", enabled: true },
        { name: "xandr", enabled: false },
      ]),
    ).toEqual(["gamma"]);
  });
});
