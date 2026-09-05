import { describe, expect, it } from "vitest";
import { plural } from "./plural";

describe("plural", () => {
  it("picks the singular only for exactly one", () => {
    expect(plural(1, "row")).toBe("1 row");
    expect(plural(0, "row")).toBe("0 rows");
    expect(plural(2, "row")).toBe("2 rows");
  });

  it("takes an irregular plural", () => {
    expect(plural(3, "entry", "entries")).toBe("3 entries");
    expect(plural(1, "entry", "entries")).toBe("1 entry");
  });
});
