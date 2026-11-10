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

import { needsFullLoad } from "./fullLoad";
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
// (internal/webui/spa.go), plus legacy /orchestrator?tab=usage|adoption links
// (which OrchestratorInner also redirects on a full load once the admin role
// is confirmed, while non-admins fall back to Recent Tasks).
export function resolveAlias(pathname: string, search: string): string | null {
  if (pathname === "/") return "/chat";
  if (pathname === "/admin" || pathname === "/settings/admin/notifications") {
    return "/settings/admin";
  }
  if (pathname === "/orchestrator") {
    const tab = new URLSearchParams(search).get("tab");
    if (tab === "usage" || tab === "adoption") {
      return `/analytics?tab=${encodeURIComponent(tab)}`;
    }
  }
  const task = /^\/orchestrator\/tasks\/([^/]+)$/.exec(pathname);
  if (task) return `/orchestrator?task=${encodeURIComponent(decodeURIComponent(task[1]))}`;
  return null;
}

/**
 * navigate moves to href. Same-origin app paths are soft navigations;
 * anything else (another origin, an /api/* endpoint that answers with a
 * redirect) is a full load.
 */
export function navigate(href: string, opts: { replace?: boolean } = {}) {
  const url = new URL(href, window.location.href);
  if (
    url.origin !== window.location.origin ||
    url.pathname.startsWith("/api/") ||
    needsFullLoad(window.location.pathname, url.pathname)
  ) {
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
};

/** useRouter — imperative push/replace over the History API. */
export function useRouter(): AppRouter {
  return useMemo(
    () => ({
      push: (href: string) => navigate(href),
      replace: (href: string) => navigate(href, { replace: true }),
    }),
    [],
  );
}
