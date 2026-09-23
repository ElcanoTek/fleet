import { describe, expect, it } from "vitest";
import {
  browserTimeZone,
  formatTimeZoneLabel,
  isValidTimeZone,
  sameTimeZone,
  timeZoneOptions,
} from "./timezones";

describe("timezones", () => {
  it("resolves the browser zone to a valid IANA name", () => {
    expect(isValidTimeZone(browserTimeZone())).toBe(true);
  });

  it("validates zone names", () => {
    expect(isValidTimeZone("America/New_York")).toBe(true);
    expect(isValidTimeZone("UTC")).toBe(true);
    expect(isValidTimeZone("Mars/Olympus_Mons")).toBe(false);
    expect(isValidTimeZone("")).toBe(false);
  });

  it("lists UTC first and always includes the requested zones", () => {
    const zones = timeZoneOptions("America/New_York", "Etc/GMT+5");
    expect(zones[0]).toBe("UTC");
    expect(zones).toContain("America/New_York");
    expect(zones).toContain("Etc/GMT+5");
    expect(new Set(zones).size).toBe(zones.length);
  });

  it("labels a zone with its abbreviation at the given instant", () => {
    const summer = new Date(Date.UTC(2026, 6, 1, 12));
    const winter = new Date(Date.UTC(2026, 0, 15, 12));
    expect(formatTimeZoneLabel("America/New_York", summer)).toBe("America/New_York (EDT)");
    expect(formatTimeZoneLabel("America/New_York", winter)).toBe("America/New_York (EST)");
    expect(formatTimeZoneLabel("UTC", summer)).toBe("UTC");
  });

  it("treats IANA aliases as the same zone", () => {
    expect(sameTimeZone("US/Eastern", "America/New_York")).toBe(true);
    expect(sameTimeZone("Etc/UTC", "UTC")).toBe(true);
    expect(sameTimeZone("America/Chicago", "America/New_York")).toBe(false);
  });
});
