import { afterEach, expect, it, vi } from "vitest";

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
});

it.each(["en", "es", "pt"])("formats display decimals in %s without changing precision", async (language) => {
  window.history.replaceState({}, "", `/?lang=${language}`);
  vi.resetModules();
  const { formatFixed } = await import("./index");
  expect(formatFixed(1234.5, 2)).toBe(language === "en" ? "1234.50" : "1234,50");
  expect(formatFixed(0.0003, 4)).toBe(language === "en" ? "0.0003" : "0,0003");
  expect(formatFixed(0.0375, 3)).toBe(language === "en" ? "0.037" : "0,037");
});
