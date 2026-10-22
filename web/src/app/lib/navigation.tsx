"use client";

// The SPA's router: a deliberately small, in-house replacement for the three
// next/navigation + next/link surfaces this app used (usePathname, useRouter's
// push/replace, <Link>). See web/AGENTS.md ("Routing") for why it is not
// React Router: the app has a fixed, flat set of ~20 URLs, the components
// already speak Next's API shape, and keeping that shape here meant the move
// off Next changed import paths instead of every call site — with no new
// runtime dependency.
//
// Navigation is the History API. A soft navigation updates one shared
// location store; every component reading usePathname() re-renders. A full
// page load (window.location) is still the right tool for a sign-in/sign-out
// round trip, and code that already does that keeps doing it.

import { useMemo, useSyncExternalStore } from "react";

type Listener = () => void;
const listeners = new Set<Listener>();

function notify() {
  for (const l of listeners) l();
}

function subscribe(l: Listener) {
  listeners.add(l);
  if (listeners.size === 1 && typeof window !== "undefined") {
    window.addEventListener("popstate", notify);
  }
  return () => {
    listeners.delete(l);
    if (listeners.size === 0 && typeof window !== "undefined") {
      window.removeEventListener("popstate", notify);
    }
  };
}

function currentPathname(): string {
  return typeof window === "undefined" ? "/" : window.location.pathname;
}

function currentSearch(): string {
  return typeof window === "undefined" ? "" : window.location.search;
}

// Client-side aliases the server also answers with a redirect on a full load
// (internal/webui/spa.go): a soft navigation to one lands where a reload would.
export function resolveAlias(pathname: string, search: string): string | null {
  if (pathname === "/") return "/chat";
  if (pathname === "/admin") return "/settings/admin";
  const task = /^\/orchestrator\/tasks\/([^/]+)$/.exec(pathname);
  if (task) return `/orchestrator?task=${encodeURIComponent(decodeURIComponent(task[1]))}`;
  void search;
  return null;
}

/**
 * navigate moves to href. Same-origin app paths are soft navigations;
 * anything else (another origin, an /api/* endpoint that answers with a
 * redirect) is a full load.
 */
export function navigate(href: string, opts: { replace?: boolean } = {}) {
  const url = new URL(href, window.location.href);
  if (url.origin !== window.location.origin || url.pathname.startsWith("/api/")) {
    if (opts.replace) window.location.replace(url.toString());
    else window.location.assign(url.toString());
    return;
  }
  const alias = resolveAlias(url.pathname, url.search);
  const target = alias ?? `${url.pathname}${url.search}${url.hash}`;
  if (opts.replace) window.history.replaceState(null, "", target);
  else window.history.pushState(null, "", target);
  if (!url.hash) window.scrollTo?.(0, 0);
  notify();
}

/** usePathname — the current path, re-rendering on every navigation. */
export function usePathname(): string {
  return useSyncExternalStore(subscribe, currentPathname, () => "/");
}

/** useSearch — the current query string ("?a=b" or ""). */
export function useSearch(): string {
  return useSyncExternalStore(subscribe, currentSearch, () => "");
}

export type AppRouter = {
  push: (href: string) => void;
  replace: (href: string) => void;
  back: () => void;
  refresh: () => void;
  prefetch: (href: string) => void;
};

/** useRouter — Next's imperative router shape, over the History API. */
export function useRouter(): AppRouter {
  return useMemo(
    () => ({
      push: (href: string) => navigate(href),
      replace: (href: string) => navigate(href, { replace: true }),
      back: () => window.history.back(),
      refresh: () => notify(),
      prefetch: () => {},
    }),
    [],
  );
}
