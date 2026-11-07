import { describe, expect, it } from "vitest";
import { formatBytes, formatUptime } from "./lib";

// #1959: the admin Overview, Server health and Storage pages each carried
// their own copy of these. One copy lives in lib.ts now; these pin the output
// the pages (and e2e/mocked/admin-health.spec.ts) depend on.
describe("formatBytes (admin)", () => {
  it("uses adaptive precision across units", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.50 KB");
    expect(formatBytes(15 * 1024)).toBe("15.0 KB");
    expect(formatBytes(150 * 1024 * 1024)).toBe("150 MB");
    expect(formatBytes(2 * 1024 ** 4)).toBe("2.00 TB");
    expect(formatBytes(2048 * 1024 ** 4)).toBe("2048 TB");
  });

  it("appends /s for a rate", () => {
    expect(formatBytes(1536, true)).toBe("1.50 KB/s");
    expect(formatBytes(0, true)).toBe("0 B/s");
  });

  it("renders a dash for a missing or negative value", () => {
    expect(formatBytes(-1)).toBe("—");
    expect(formatBytes(Number.NaN)).toBe("—");
    expect(formatBytes(Number.POSITIVE_INFINITY, true)).toBe("—");
  });
});

describe("formatUptime", () => {
  it("shows the two largest units", () => {
    expect(formatUptime(59)).toBe("0m");
    expect(formatUptime(12 * 60)).toBe("12m");
    expect(formatUptime(4 * 3600 + 12 * 60)).toBe("4h 12m");
    expect(formatUptime(3 * 86400 + 4 * 3600 + 30 * 60)).toBe("3d 4h");
  });

  it("renders a dash when unknown", () => {
    expect(formatUptime(0)).toBe("—");
    expect(formatUptime(-5)).toBe("—");
    expect(formatUptime(Number.NaN)).toBe("—");
  });
});
