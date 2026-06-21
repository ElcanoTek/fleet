import { describe, expect, it, beforeEach, afterEach, vi } from "vitest";
import { currentBuildId, BUILD_ID_HEADER } from "./buildId";

describe("buildId", () => {
  const originalEnv = process.env;

  beforeEach(() => {
    vi.resetModules();
    process.env = { ...originalEnv };
  });

  afterEach(() => {
    process.env = originalEnv;
  });

  it("returns process.env.NEXT_PUBLIC_BUILD_ID when it is set", () => {
    process.env.NEXT_PUBLIC_BUILD_ID = "test-build-123";
    expect(currentBuildId()).toBe("test-build-123");
  });

  it("returns 'dev' when process.env.NEXT_PUBLIC_BUILD_ID is not set", () => {
    delete process.env.NEXT_PUBLIC_BUILD_ID;
    expect(currentBuildId()).toBe("dev");
  });

  it("exports BUILD_ID_HEADER with the correct value", () => {
    expect(BUILD_ID_HEADER).toBe("X-App-Version");
  });
});
