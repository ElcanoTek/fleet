import * as React from "react"

export type FlagTheme = "light" | "dark"

function readTheme(): FlagTheme {
  if (typeof document === "undefined") return "dark"
  // Flag's contract: html[data-theme] is set by the theme controller; the
  // tokens are dark unless the root says light.
  return document.documentElement.getAttribute("data-theme") === "light"
    ? "light"
    : "dark"
}

function subscribe(onChange: () => void) {
  const observer = new MutationObserver(onChange)
  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["data-theme"],
  })
  return () => observer.disconnect()
}

/** The resolved Flag theme (html[data-theme]), kept in sync with the toggle. */
export function useFlagTheme(): FlagTheme {
  return React.useSyncExternalStore(subscribe, readTheme, () => "dark")
}
