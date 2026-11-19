import { sidebarSettled } from "./store";
import { canOpen } from "./targets";
import { fetchMe } from "@/app/shared/lib/account";
import type { SidebarItem } from "./layout";

// Home (docs/SIDEBAR.md): the sidebar item fleet opens to. It applies only
// when fleet is OPENED — the page was loaded at /chat (where "/" and sign-in
// land) with no link asking for something specific — and only once per page
// load. Moving around inside fleet afterwards never jumps back to Home, so
// clicking Chat still opens Chat.
//
// The address is captured when the app's code first runs (main.tsx calls
// captureEntryAddress), before any page strips its link parameters.

// Parameters that are about how the page looks, not what it opens.
const NEUTRAL_PARAMS = new Set(["lang"]);

let entry: { path: string; search: string } | null = null;
let used = false;

/** captureEntryAddress records the address this page load opened at (once). */
export function captureEntryAddress() {
  if (entry || typeof window === "undefined") return;
  entry = { path: window.location.pathname, search: window.location.search };
}

/** entryHomeEligible: this page load opened fleet itself, and Home has not been applied yet. */
export function entryHomeEligible(): boolean {
  if (used || !entry) return false;
  if (entry.path !== "/chat" && entry.path !== "/") return false;
  for (const key of new URLSearchParams(entry.search).keys()) {
    if (!NEUTRAL_PARAMS.has(key)) return false;
  }
  return true;
}

/** markEntryHomeUsed: the first boot of the page is done; Home is not applied again. */
export function markEntryHomeUsed() {
  used = true;
}

/**
 * resolveEntryHome: the Home item, if the person (or their workspace) set
 * one. Waits for the layout read at most `timeoutMs`: a slow read opens the
 * usual most recent chat rather than holding the page.
 */
export async function resolveEntryHome(timeoutMs: number): Promise<SidebarItem | null> {
  const started = Date.now();
  const state = await sidebarSettled(timeoutMs);
  // Home comes only from a layout the server answered with: a cached copy
  // (shown while the read is slow) may name a Home changed or cleared since
  // on another device.
  if (state.status !== "ready") return null;
  const home = state.layout.home;
  const remaining = () => Math.max(0, timeoutMs - (Date.now() - started));
  const within = <T,>(p: Promise<T>): Promise<T | null> =>
    Promise.race([p, new Promise<null>((resolve) => setTimeout(() => resolve(null), remaining()))]);
  if (!home) return null;
  const item = state.layout.items.find((i) => i.id === home);
  // A hidden shortcut is a start page that is not kept at the top of the
  // sidebar; a hidden destination is one the person does not use.
  if (!item || (item.hidden && item.kind === "builtin") || item.id === "chat" || item.id === "patrols") return null;
  const fromWorkspace = state.source !== "user";
  // Only its owner and admins can open a task, and only admins Analytics:
  // never a workspace Home.
  if (fromWorkspace && (item.kind === "task" || (item.kind === "page" && item.ref === "analytics"))) return null;
  // A personal Analytics Home outlives the role that set it: follow it only
  // while this account is still an admin, never into "admin access required".
  if (item.kind === "page" && item.ref === "analytics") {
    const me = await within(fetchMe().catch(() => null));
    if (!me?.admin) return null;
  }
  if (item.kind === "chat" || item.kind === "project") {
    // Home goes only where this account can go. A workspace Home that is not
    // confirmed openable (in time) is skipped, so nobody lands on an error
    // page instead of their chats; the person's own Home is skipped only when
    // it is known to be gone.
    const ok = await within(canOpen(item));
    if (ok === false || (ok === null && fromWorkspace)) return null;
  }
  return item;
}

/** Test hook: forget the captured address and whether Home was applied. */
export function resetEntryHomeForTests() {
  used = false;
  entry = null;
}
