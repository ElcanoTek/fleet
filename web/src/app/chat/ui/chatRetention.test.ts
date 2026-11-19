import { describe, expect, it } from "vitest";
import { daysUntilDeletion, expiresInDays, nearCap, retentionPolicy } from "./chatRetention";

const DAY = 86_400;
const NOW = 1_800_000_000;
const idleFor = (days: number) => NOW - days * DAY;

describe("retentionPolicy", () => {
  it("uses the deployment's numbers", () => {
    expect(
      retentionPolicy({ conversationTtlDays: 30, conversationExpiryWarningDays: 7, conversationUnpinnedCap: 200 }),
    ).toEqual({ ttlDays: 30, warningDays: 7, cap: 200 });
  });

  it("assumes the default window and makes no countdown promise when the server does not say", () => {
    expect(retentionPolicy({})).toEqual({ ttlDays: 90, warningDays: 0, cap: undefined });
  });

  it("holds the warning inside the window", () => {
    expect(retentionPolicy({ conversationTtlDays: 7, conversationExpiryWarningDays: 14 }).warningDays).toBe(6);
    expect(retentionPolicy({ conversationTtlDays: 1, conversationExpiryWarningDays: 14 }).warningDays).toBe(0);
  });
});

describe("daysUntilDeletion", () => {
  it("counts whole days, rounded up", () => {
    expect(daysUntilDeletion(idleFor(0), 90, NOW)).toBe(90);
    expect(daysUntilDeletion(idleFor(85), 90, NOW)).toBe(5);
    expect(daysUntilDeletion(idleFor(85) - 3600, 90, NOW)).toBe(5);
    expect(daysUntilDeletion(idleFor(89) - 3600, 90, NOW)).toBe(1);
  });

  it("is 0 once the window has passed", () => {
    expect(daysUntilDeletion(idleFor(90), 90, NOW)).toBe(0);
    expect(daysUntilDeletion(idleFor(200), 90, NOW)).toBe(0);
  });
});

describe("expiresInDays", () => {
  const policy = { ttlDays: 90, warningDays: 14 };

  it("says nothing until the warning window", () => {
    expect(expiresInDays(idleFor(0), policy, NOW)).toBeNull();
    expect(expiresInDays(idleFor(75), policy, NOW)).toBeNull();
  });

  it("gives the days left inside it", () => {
    expect(expiresInDays(idleFor(76), policy, NOW)).toBe(14);
    expect(expiresInDays(idleFor(88), policy, NOW)).toBe(2);
    expect(expiresInDays(idleFor(95), policy, NOW)).toBe(0);
  });

  it("is off when the warning is off", () => {
    expect(expiresInDays(idleFor(89), { ttlDays: 90, warningDays: 0 }, NOW)).toBeNull();
  });
});

describe("nearCap", () => {
  it("is true within a tenth of the cap", () => {
    expect(nearCap(449, 500)).toBe(false);
    expect(nearCap(450, 500)).toBe(true);
    expect(nearCap(500, 500)).toBe(true);
  });

  it("still warns before a small cap", () => {
    expect(nearCap(3, 5)).toBe(false);
    expect(nearCap(4, 5)).toBe(true);
  });

  it("is false with no cap", () => {
    expect(nearCap(10_000, undefined)).toBe(false);
    expect(nearCap(10_000, 0)).toBe(false);
  });
});
