import { afterEach, describe, expect, it, vi } from "vitest";
import { currentBuildId, BUILD_ID_HEADER } from "./buildId";

describe("buildId", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it("returns import.meta.env.VITE_BUILD_ID when it is set", () => {
    vi.stubEnv("VITE_BUILD_ID", "test-build-123");
    expect(currentBuildId()).toBe("test-build-123");
  });

  it("returns 'dev' when VITE_BUILD_ID is not set", () => {
    vi.stubEnv("VITE_BUILD_ID", undefined);
    expect(currentBuildId()).toBe("dev");
  });

  it("exports BUILD_ID_HEADER with the correct value", () => {
    expect(BUILD_ID_HEADER).toBe("X-App-Version");
  });
});
