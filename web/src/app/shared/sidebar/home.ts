import { sidebarSettled } from "./store";
import { canOpen } from "./targets";
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
  const home = state.layout.home;
  if (!home) return null;
  const item = state.layout.items.find((i) => i.id === home);
  if (!item || item.hidden || item.id === "chat" || item.id === "patrols") return null;
  const fromWorkspace = state.source !== "user";
  // Only its owner and admins can open a task: never a workspace Home.
  if (item.kind === "task" && fromWorkspace) return null;
  if (item.kind === "chat" || item.kind === "project") {
    // Home goes only where this account can go. A workspace Home that is not
    // confirmed openable (in time) is skipped, so nobody lands on an error
    // page instead of their chats; the person's own Home is skipped only when
    // it is known to be gone.
    const left = Math.max(0, timeoutMs - (Date.now() - started));
    const ok = await Promise.race([
      canOpen(item),
      new Promise<null>((resolve) => setTimeout(() => resolve(null), left)),
    ]);
    if (ok === false || (ok === null && fromWorkspace)) return null;
  }
  return item;
}

/** Test hook: forget the captured address and whether Home was applied. */
export function resetEntryHomeForTests() {
  used = false;
  entry = null;
}
