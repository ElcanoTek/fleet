import { describe, expect, it } from "vitest";

import { stripAnsiCodes } from "./format";

const ESC = String.fromCharCode(27); // ESC (\x1b), kept out of the source as a raw byte

describe("stripAnsiCodes", () => {
  it("strips real ESC[..m terminal color codes", () => {
    expect(stripAnsiCodes(`${ESC}[31mred${ESC}[0m`)).toBe("red");
    expect(stripAnsiCodes(`${ESC}[1;32mok${ESC}[0m done`)).toBe("ok done");
  });

  it("does NOT mangle bare [..m text that has no ESC byte", () => {
    // Regression: a second bare-bracket pass turned legitimate log text like
    // "claude-opus-4-8[1m]" into "claude-opus-4-8]" and "[42m headroom" into
    // " headroom". Only ESC-prefixed codes are real terminal colors.
    expect(stripAnsiCodes("claude-opus-4-8[1m]")).toBe("claude-opus-4-8[1m]");
    expect(stripAnsiCodes("[42m headroom")).toBe("[42m headroom");
  });

  it("returns empty string for null/undefined", () => {
    expect(stripAnsiCodes(null)).toBe("");
    expect(stripAnsiCodes(undefined)).toBe("");
  });
});
