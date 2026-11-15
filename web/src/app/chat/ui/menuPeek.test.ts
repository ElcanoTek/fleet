import { describe, expect, it } from "vitest";

import { menuPeekHeight } from "./menuPeek";

// Four 44px rows, 2px apart, starting 5px down.
const rows = [0, 1, 2, 3].map((i) => ({ top: 5 + i * 46, height: 44 }));

describe("menuPeekHeight", () => {
  it("leaves a menu alone when every row fits", () => {
    expect(menuPeekHeight(200, rows)).toBeNull();
  });

  it("leaves a menu alone when its edge already cuts a row", () => {
    // Row 4 starts at 143: 20px of it shows.
    expect(menuPeekHeight(163, rows)).toBeNull();
  });

  it("shortens a menu whose edge lands in the gap between rows", () => {
    // Row 3 ends at 141 and row 4 starts at 143: nothing of row 4 shows.
    expect(menuPeekHeight(142, rows)).toBe(97 + 22);
  });

  it("shortens a menu that shows only a sliver of the next row", () => {
    expect(menuPeekHeight(148, rows)).toBe(97 + 22);
  });

  it("shortens a menu that hides only a sliver of a row", () => {
    // Row 4 ends at 187: all but 5px of it shows.
    expect(menuPeekHeight(182, rows)).toBe(143 + 22);
  });

  it("does nothing when not even the first row can be cut", () => {
    expect(menuPeekHeight(8, rows)).toBeNull();
  });
});
