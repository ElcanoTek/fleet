import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CopyChip } from "./panels";

const original = Object.getOwnPropertyDescriptor(navigator, "clipboard");

function setClipboard(writeText: (t: string) => Promise<void>) {
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
}

afterEach(() => {
  cleanup();
  if (original) Object.defineProperty(navigator, "clipboard", original);
  else delete (navigator as { clipboard?: unknown }).clipboard;
});

describe("CopyChip", () => {
  it("announces a copy that happened", async () => {
    const writeText = vi.fn(async () => {});
    setClipboard(writeText);
    render(<CopyChip name="triage" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy /triage to clipboard" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Copied /triage to clipboard"));
    expect(writeText).toHaveBeenCalledWith("/triage");
  });

  it("does not say Copied when the clipboard refuses (#2001)", async () => {
    const writeText = vi.fn(async () => Promise.reject(new DOMException("denied", "NotAllowedError")));
    setClipboard(writeText);
    render(<CopyChip name="triage" />);
    fireEvent.click(screen.getByRole("button", { name: "Copy /triage to clipboard" }));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    await Promise.resolve();
    expect(screen.getByRole("status")).toHaveTextContent("");
  });
});
