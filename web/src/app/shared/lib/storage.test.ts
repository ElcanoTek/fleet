import { afterEach, describe, expect, it, vi } from "vitest";
import {
  SEND_KEY_STORAGE,
  readPref,
  readSendOnEnter,
  removePref,
  writePref,
  writeSendOnEnter,
} from "./storage";

afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe("storage prefs", () => {
  it("round-trips through local and session storage", () => {
    expect(readPref("k")).toBeNull();
    expect(writePref("k", "v")).toBe(true);
    expect(readPref("k")).toBe("v");
    expect(window.localStorage.getItem("k")).toBe("v");
    removePref("k");
    expect(readPref("k")).toBeNull();

    expect(writePref("s", "1", "session")).toBe(true);
    expect(window.sessionStorage.getItem("s")).toBe("1");
    expect(readPref("s")).toBeNull();
    expect(readPref("s", "session")).toBe("1");
  });

  it("swallows a storage that throws on every call", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(readPref("k")).toBeNull();
    expect(writePref("k", "v")).toBe(false);
    expect(() => removePref("k")).not.toThrow();
    expect(readSendOnEnter()).toBe(true);
    expect(writeSendOnEnter(false)).toBe(false);
  });

  it("swallows a storage getter that throws (cookies blocked)", () => {
    vi.spyOn(window, "localStorage", "get").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(readPref("k")).toBeNull();
    expect(writePref("k", "v")).toBe(false);
    expect(() => removePref("k")).not.toThrow();
  });
});

describe("send key", () => {
  it("defaults to Enter and stores the Ctrl/Cmd+Enter choice", () => {
    expect(readSendOnEnter()).toBe(true);
    writeSendOnEnter(false);
    expect(window.localStorage.getItem(SEND_KEY_STORAGE)).toBe("ctrl+enter");
    expect(readSendOnEnter()).toBe(false);
    writeSendOnEnter(true);
    expect(window.localStorage.getItem(SEND_KEY_STORAGE)).toBe("enter");
    expect(readSendOnEnter()).toBe(true);
  });
});
