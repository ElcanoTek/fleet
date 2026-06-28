import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, cleanup, act } from "@testing-library/react";
import {
  matchesShortcut,
  useKeyboardShortcuts,
  isMacPlatform,
  type KeyboardShortcut,
} from "./useKeyboardShortcuts";

// Force a deterministic platform for the modifier tests. navigator.platform is
// what isMacPlatform / platformModActive read; overriding it lets us assert both
// the Mac (⌘) and non-Mac (Ctrl) branches.
function setPlatform(value: string) {
  Object.defineProperty(window.navigator, "platform", {
    value,
    configurable: true,
  });
}

// Build a KeyboardEvent the way matchesShortcut expects, with a chosen target so
// the typing-suppression logic can be exercised.
function keyEvent(
  init: KeyboardEventInit & { key: string },
  target?: EventTarget,
): KeyboardEvent {
  const event = new KeyboardEvent("keydown", { ...init, bubbles: true, cancelable: true });
  if (target) Object.defineProperty(event, "target", { value: target, configurable: true });
  return event;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("isMacPlatform", () => {
  it("detects macOS from navigator.platform", () => {
    setPlatform("MacIntel");
    expect(isMacPlatform()).toBe(true);
  });

  it("returns false on non-Apple platforms", () => {
    setPlatform("Win32");
    expect(isMacPlatform()).toBe(false);
  });
});

describe("matchesShortcut", () => {
  it("matches a bare key case-insensitively when no modifier is required", () => {
    setPlatform("Win32");
    const sc: KeyboardShortcut = { key: "?", handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "?" }), sc)).toBe(true);
    expect(matchesShortcut(keyEvent({ key: "k" }), sc)).toBe(false);
  });

  it("requires the platform modifier when mod:true — Ctrl off a Mac", () => {
    setPlatform("Win32");
    const sc: KeyboardShortcut = { key: "k", mod: true, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "k", ctrlKey: true }), sc)).toBe(true);
    // metaKey is NOT the platform mod off a Mac, so it must not match.
    expect(matchesShortcut(keyEvent({ key: "k", metaKey: true }), sc)).toBe(false);
    // No modifier at all → no match.
    expect(matchesShortcut(keyEvent({ key: "k" }), sc)).toBe(false);
  });

  it("requires the platform modifier when mod:true — Cmd on a Mac", () => {
    setPlatform("MacIntel");
    const sc: KeyboardShortcut = { key: "k", mod: true, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "k", metaKey: true }), sc)).toBe(true);
    // Ctrl is NOT the platform mod on a Mac.
    expect(matchesShortcut(keyEvent({ key: "k", ctrlKey: true }), sc)).toBe(false);
  });

  it("rejects a bare shortcut that arrives with the platform modifier held", () => {
    setPlatform("Win32");
    const sc: KeyboardShortcut = { key: "?", handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "?", ctrlKey: true }), sc)).toBe(false);
  });

  it("honors an explicit shift requirement", () => {
    setPlatform("Win32");
    const needsShift: KeyboardShortcut = { key: "n", mod: true, shift: true, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "n", ctrlKey: true, shiftKey: true }), needsShift)).toBe(true);
    expect(matchesShortcut(keyEvent({ key: "n", ctrlKey: true }), needsShift)).toBe(false);

    const forbidsShift: KeyboardShortcut = { key: "n", mod: true, shift: false, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "n", ctrlKey: true }), forbidsShift)).toBe(true);
    expect(matchesShortcut(keyEvent({ key: "n", ctrlKey: true, shiftKey: true }), forbidsShift)).toBe(false);
  });

  it("suppresses bare shortcuts while typing in a text field", () => {
    setPlatform("Win32");
    const input = document.createElement("input");
    const textarea = document.createElement("textarea");
    const sc: KeyboardShortcut = { key: "?", handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "?" }, input), sc)).toBe(false);
    expect(matchesShortcut(keyEvent({ key: "?" }, textarea), sc)).toBe(false);
  });

  it("still fires a modifier shortcut from inside an input only when allowInInput is set", () => {
    setPlatform("Win32");
    const input = document.createElement("input");
    const blocked: KeyboardShortcut = { key: "f", mod: true, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "f", ctrlKey: true }, input), blocked)).toBe(false);

    const allowed: KeyboardShortcut = { key: "k", mod: true, allowInInput: true, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "k", ctrlKey: true }, input), allowed)).toBe(true);
  });

  it("never matches a disabled shortcut", () => {
    setPlatform("Win32");
    const sc: KeyboardShortcut = { key: "?", enabled: false, handler: () => {} };
    expect(matchesShortcut(keyEvent({ key: "?" }), sc)).toBe(false);
  });
});

describe("useKeyboardShortcuts", () => {
  beforeEach(() => setPlatform("Win32"));

  it("invokes the first matching handler and preventDefaults the event", () => {
    const handler = vi.fn();
    renderHook(() => useKeyboardShortcuts([{ key: "?", handler }]));

    const event = keyEvent({ key: "?" }, document.body);
    act(() => {
      window.dispatchEvent(event);
    });
    expect(handler).toHaveBeenCalledTimes(1);
    expect(event.defaultPrevented).toBe(true);
  });

  it("does not fire a bare shortcut dispatched from a focused input", () => {
    const handler = vi.fn();
    renderHook(() => useKeyboardShortcuts([{ key: "?", handler }]));

    const input = document.createElement("input");
    document.body.appendChild(input);
    act(() => {
      window.dispatchEvent(keyEvent({ key: "?" }, input));
    });
    expect(handler).not.toHaveBeenCalled();
    input.remove();
  });

  it("only fires the first matching shortcut when several would match", () => {
    const first = vi.fn();
    const second = vi.fn();
    renderHook(() =>
      useKeyboardShortcuts([
        { key: "k", mod: true, handler: first },
        { key: "k", mod: true, handler: second },
      ]),
    );
    act(() => {
      window.dispatchEvent(keyEvent({ key: "k", ctrlKey: true }, document.body));
    });
    expect(first).toHaveBeenCalledTimes(1);
    expect(second).not.toHaveBeenCalled();
  });

  it("reads the latest shortcut list without re-subscribing", () => {
    const a = vi.fn();
    const b = vi.fn();
    const { rerender } = renderHook(({ shortcuts }) => useKeyboardShortcuts(shortcuts), {
      initialProps: { shortcuts: [{ key: "?", handler: a }] as KeyboardShortcut[] },
    });
    rerender({ shortcuts: [{ key: "?", handler: b }] as KeyboardShortcut[] });
    act(() => {
      window.dispatchEvent(keyEvent({ key: "?" }, document.body));
    });
    expect(a).not.toHaveBeenCalled();
    expect(b).toHaveBeenCalledTimes(1);
  });

  it("removes its listener on unmount", () => {
    const handler = vi.fn();
    const { unmount } = renderHook(() => useKeyboardShortcuts([{ key: "?", handler }]));
    unmount();
    act(() => {
      window.dispatchEvent(keyEvent({ key: "?" }, document.body));
    });
    expect(handler).not.toHaveBeenCalled();
  });
});
