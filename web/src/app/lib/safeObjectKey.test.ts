import { describe, expect, it } from "vitest";
import { deleteOwn, isSafeObjectKey, setOwn } from "./safeObjectKey";

describe("isSafeObjectKey", () => {
  it("accepts conversation-shaped keys", () => {
    expect(isSafeObjectKey("550e8400-e29b-41d4-a716-446655440000")).toBe(true);
    expect(isSafeObjectKey("__pending__:1")).toBe(true);
  });

  it("rejects prototype-polluting names", () => {
    expect(isSafeObjectKey("__proto__")).toBe(false);
    expect(isSafeObjectKey("constructor")).toBe(false);
    expect(isSafeObjectKey("prototype")).toBe(false);
  });
});

describe("setOwn / deleteOwn", () => {
  it("writes and deletes a safe key", () => {
    const obj: Record<string, number> = {};
    setOwn(obj, "a", 1);
    expect(obj.a).toBe(1);
    deleteOwn(obj, "a");
    expect("a" in obj).toBe(false);
  });

  it("does not write prototype-polluting names", () => {
    const obj: Record<string, unknown> = {};
    setOwn(obj, "__proto__", { polluted: true });
    expect(Object.prototype.hasOwnProperty.call(obj, "__proto__")).toBe(false);
    expect(({} as { polluted?: boolean }).polluted).toBeUndefined();
  });
});
