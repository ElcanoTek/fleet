"use client";

import { useEffect, useRef } from "react";

// useKeyboardShortcuts — a single, declarative keyboard-shortcut layer for the
// unified shell (#306). Before this hook each shortcut was an ad-hoc
// `window.addEventListener("keydown", …)` effect scattered through
// chat-experience.tsx with no shared discoverability. Callers now describe their
// shortcuts as data and register them all through one listener, which:
//
//   - resolves the platform "mod" key once (⌘ on macOS, Ctrl elsewhere) so a
//     shortcut declared with `mod: true` fires on Cmd on a Mac and Ctrl on
//     Windows/Linux without the caller branching;
//   - refuses to hijack typing: while focus is in an <input>, <textarea>,
//     <select>, or a contentEditable element, only modifier-qualified shortcuts
//     (mod/ctrl/meta/alt) fire — bare-letter shortcuts (`?`, `j`, `k`) are
//     suppressed so they land in the field as text. The composer's own
//     Enter / Shift+Enter handling lives on the textarea and is untouched.
//
// The hook owns nothing visual; it only invokes the handlers the caller
// supplies. The companion `KeyboardShortcutsOverlay` renders the same list as a
// discoverable help modal.

export interface KeyboardShortcut {
  /** The KeyboardEvent.key to match, compared case-insensitively (e.g. "k", "?", "Enter"). */
  key: string;
  /**
   * Require the platform modifier (⌘ on macOS, Ctrl elsewhere). When true the
   * shortcut fires only with that modifier held; when false/undefined it fires
   * only with no platform modifier held.
   */
  mod?: boolean;
  /** Require (true) or forbid (false) the Shift modifier. Undefined ignores Shift. */
  shift?: boolean;
  /** Require (true) or forbid (false) the Alt/Option modifier. Undefined ignores Alt. */
  alt?: boolean;
  /** Invoked when the shortcut matches. The matched event is passed for preventDefault etc. */
  handler: (event: KeyboardEvent) => void;
  /**
   * When true, the shortcut still fires while a text field is focused. Use
   * sparingly — only for modifier-qualified actions that should work mid-typing
   * (e.g. ⌘K to open search from inside the composer). Bare-letter shortcuts
   * must leave this false so they don't eat keystrokes.
   */
  allowInInput?: boolean;
  /** Whether the shortcut is currently active. Defaults to true. */
  enabled?: boolean;
}

/** Returns true when the platform modifier (⌘ on macOS, Ctrl elsewhere) is held. */
export function platformModActive(event: KeyboardEvent): boolean {
  return isMacPlatform() ? event.metaKey : event.ctrlKey;
}

/** True on macOS / iOS, where the convention is ⌘ rather than Ctrl. */
export function isMacPlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  // navigator.platform is deprecated but still the most reliable signal here and
  // is what the rest of chat-experience uses for the ⌘K hint; userAgent is the
  // fallback for engines that have already dropped platform.
  const probe = navigator.platform || navigator.userAgent || "";
  return /Mac|iPhone|iPad|iPod/i.test(probe);
}

/** True when the event target is a text-entry element we must not hijack. */
function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    target.isContentEditable
  );
}

/**
 * Returns true when `event` matches `shortcut`. Exported for unit tests and for
 * the overlay's "press a key" affordances; the hook uses it internally.
 */
export function matchesShortcut(event: KeyboardEvent, shortcut: KeyboardShortcut): boolean {
  if (shortcut.enabled === false) return false;
  if (event.key.toLowerCase() !== shortcut.key.toLowerCase()) return false;

  const modWanted = shortcut.mod ?? false;
  if (modWanted !== platformModActive(event)) return false;

  if (shortcut.shift !== undefined && shortcut.shift !== event.shiftKey) return false;
  if (shortcut.alt !== undefined && shortcut.alt !== event.altKey) return false;

  // Suppress bare (non-modifier) shortcuts while typing unless explicitly
  // opted in. Modifier-qualified shortcuts may fire mid-typing only when the
  // caller marks them allowInInput.
  if (isTypingTarget(event.target)) {
    if (!shortcut.allowInInput) return false;
  }
  return true;
}

/**
 * Registers `shortcuts` on a single window keydown listener. The first matching
 * shortcut wins; its handler runs after `preventDefault()` so the browser
 * default (e.g. ⌘K's address-bar focus) is suppressed for handled keys only.
 *
 * The list is read through a ref so callers can pass a freshly-built array each
 * render (the common case — handlers close over component state) without
 * re-subscribing the listener every render.
 */
export function useKeyboardShortcuts(shortcuts: KeyboardShortcut[]): void {
  const shortcutsRef = useRef(shortcuts);
  // Written in an effect (never during render) so it doesn't trip the
  // react-hooks refs rule; the listener always reads the latest committed list.
  useEffect(() => {
    shortcutsRef.current = shortcuts;
  });

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      for (const shortcut of shortcutsRef.current) {
        if (matchesShortcut(event, shortcut)) {
          event.preventDefault();
          shortcut.handler(event);
          return;
        }
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);
}
