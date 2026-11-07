import { afterEach, describe, expect, it, vi } from "vitest";
import { copyText } from "./clipboard";

const original = Object.getOwnPropertyDescriptor(navigator, "clipboard");

function setClipboard(value: unknown) {
  Object.defineProperty(navigator, "clipboard", { value, configurable: true });
}

afterEach(() => {
  if (original) Object.defineProperty(navigator, "clipboard", original);
  else delete (navigator as { clipboard?: unknown }).clipboard;
});

describe("copyText", () => {
  it("writes the text and answers true", async () => {
    const writeText = vi.fn(async () => {});
    setClipboard({ writeText });
    await expect(copyText("hello")).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith("hello");
  });

  it("answers false, never rejecting, when the write is refused", async () => {
    setClipboard({ writeText: vi.fn(async () => Promise.reject(new DOMException("denied", "NotAllowedError"))) });
    await expect(copyText("x")).resolves.toBe(false);
  });

  it("answers false when writeText throws synchronously", async () => {
    setClipboard({
      writeText: () => {
        throw new Error("boom");
      },
    });
    await expect(copyText("x")).resolves.toBe(false);
  });

  it("answers false when there is no clipboard", async () => {
    setClipboard(undefined);
    await expect(copyText("x")).resolves.toBe(false);
    setClipboard({});
    await expect(copyText("x")).resolves.toBe(false);
  });

  it("calls writeText synchronously, inside the user gesture", () => {
    const writeText = vi.fn(async () => {});
    setClipboard({ writeText });
    void copyText("now");
    expect(writeText).toHaveBeenCalledTimes(1);
  });
});
