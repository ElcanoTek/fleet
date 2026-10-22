// needsFullLoad — whether a hop between two paths must be a real page load
// rather than a client-side navigation: either end is a route whose page the
// server decides (the bootstrap's fullLoadRoutes, from Go's servePage —
// internal/webui/spa.go fullLoadRoutes). E.g. leaving /no-access for /chat
// must re-run the membership gate, which a client-side hop would skip.
//
// Its own module (not navigation.tsx) so tests that stub the router hooks
// still get the real rule.

import { getBootstrap } from "./bootstrap";

export function needsFullLoad(fromPath: string, toPath: string): boolean {
  const routes = getBootstrap().fullLoadRoutes ?? [];
  const hit = (p: string) => routes.some((r) => (r.endsWith("/") ? p.startsWith(r) : p === r));
  return hit(fromPath) || hit(toPath);
}
