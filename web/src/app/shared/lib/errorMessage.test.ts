import { describe, expect, it } from "vitest";
import { errorMessage } from "./errorMessage";

describe("errorMessage", () => {
  it("answers an Error's message", () => {
    expect(errorMessage(new Error("db locked"), "fallback")).toBe("db locked");
    expect(errorMessage(new TypeError("Failed to fetch"), "fallback")).toBe("Failed to fetch");
  });

  it("answers the fallback for an Error with no message", () => {
    expect(errorMessage(new Error(""), "fallback")).toBe("fallback");
  });

  it("answers the fallback for anything that is not an Error", () => {
    expect(errorMessage("a string", "fallback")).toBe("fallback");
    expect(errorMessage(undefined, "fallback")).toBe("fallback");
    expect(errorMessage(null, "fallback")).toBe("fallback");
    expect(errorMessage({ message: "lookalike" }, "fallback")).toBe("fallback");
  });
});
