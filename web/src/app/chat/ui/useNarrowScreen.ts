import { useSyncExternalStore } from "react";

// useNarrowScreen reports whether the window is phone-width (the same 640px
// line Tailwind's `sm:` draws). For the few places where a phone needs a
// different STRUCTURE, not just different CSS — the reply footer renders its
// occasional actions inline on a desktop and inside a bottom menu on a phone.
// Where matchMedia is missing (tests, old engines) it answers "not narrow".
const QUERY = "(max-width: 640px)";

function subscribe(onChange: () => void): () => void {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
    return () => {};
  }
  const mq = window.matchMedia(QUERY);
  mq.addEventListener("change", onChange);
  return () => mq.removeEventListener("change", onChange);
}

function snapshot(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia(QUERY).matches
  );
}

export function useNarrowScreen(): boolean {
  return useSyncExternalStore(subscribe, snapshot, () => false);
}
