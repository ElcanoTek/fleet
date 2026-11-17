import { useSyncExternalStore } from "react";
import { readPref, writePref } from "@/app/shared/lib/storage";
import { getBootstrap } from "@/app/lib/bootstrap";
import { BUILTIN_LAYOUT, normalizeLayout, sameLayout, type SidebarLayout } from "./layout";

// The sidebar layout store (docs/SIDEBAR.md): one module-level copy shared by
// every rail on the page (chat, background tasks, settings, guides, analytics)
// and the Customize panel, so an edit shows in the rail as it is made.
//
//   - Read once per page from GET /api/sidebar: the person's own layout and
//     the workspace default an admin set. Effective = own ?? workspace ??
//     BUILTIN_LAYOUT.
//   - The last effective layout is also kept in browser storage, so a page
//     load paints the person's rail at once instead of flashing the default
//     until the read lands. The copy is tagged with the account that saved
//     it and used only by that account: on a shared browser the next person
//     never sees (or is sent Home by) the previous one's sidebar. The server
//     stays the source of truth.
//   - Edits apply at once and are saved with a short debounce (PUT); a run of
//     edits is one save. A failed save keeps the edit on screen and says so.
//   - When the server cannot be reached (or is older than this feature) the
//     rail renders the default and the panel says changes cannot be saved.

const CACHE_KEY = "fleet.sidebar.layout.v2";

/** The signed-in account, as the page was served; "" when unknown. */
function sessionEmail(): string {
  try {
    return (getBootstrap().session?.email ?? "").trim().toLowerCase();
  } catch {
    return "";
  }
}
const SAVE_DELAY_MS = 450;

export type SidebarSource = "user" | "workspace" | "builtin";
export type SidebarStatus = "loading" | "ready" | "unavailable";

export type SidebarState = {
  status: SidebarStatus;
  /** The person's own saved layout, or null when they follow the default. */
  own: SidebarLayout | null;
  /** The workspace default an admin set, or null for fleet's built-in one. */
  workspace: SidebarLayout | null;
  /** What the rail renders. */
  layout: SidebarLayout;
  source: SidebarSource;
  saving: boolean;
  saveError: boolean;
};

function readCache(): SidebarLayout | null {
  const email = sessionEmail();
  const raw = readPref(CACHE_KEY);
  if (!raw || !email) return null;
  try {
    const cached = JSON.parse(raw) as { email?: unknown; layout?: unknown };
    if (cached.email !== email) return null;
    return normalizeLayout(cached.layout);
  } catch {
    return null;
  }
}

function writeCache(layout: SidebarLayout) {
  const email = sessionEmail();
  if (!email) return;
  writePref(CACHE_KEY, JSON.stringify({ email, layout }));
}

function initialState(): SidebarState {
  const cached = typeof window === "undefined" ? null : readCache();
  return {
    status: "loading",
    own: null,
    workspace: null,
    layout: cached ?? BUILTIN_LAYOUT,
    source: "builtin",
    saving: false,
    saveError: false,
  };
}

let state: SidebarState = initialState();
const listeners = new Set<() => void>();
let loadPromise: Promise<void> | null = null;
let saveTimer: ReturnType<typeof setTimeout> | null = null;
// Bumped by every local edit: a read or save answer that started before the
// latest edit must not overwrite it.
let editGen = 0;

function emit(next: Partial<SidebarState>) {
  state = { ...state, ...next };
  if (next.layout) writeCache(state.layout);
  for (const l of listeners) l();
}

function effective(own: SidebarLayout | null, workspace: SidebarLayout | null): { layout: SidebarLayout; source: SidebarSource } {
  if (own) return { layout: own, source: "user" };
  if (workspace) return { layout: workspace, source: "workspace" };
  return { layout: BUILTIN_LAYOUT, source: "builtin" };
}

/** loadSidebar reads the layouts once per page (every caller shares the read). */
export function loadSidebar(): Promise<void> {
  if (loadPromise) return loadPromise;
  const gen = editGen;
  loadPromise = (async () => {
    try {
      const res = await fetch("/api/sidebar", { cache: "no-store" });
      if (!res.ok) throw new Error(String(res.status));
      const data = (await res.json()) as { layout?: unknown; workspace_default?: unknown; rev?: unknown };
      learnRev(data.rev);
      const own = data.layout ? normalizeLayout(data.layout) : null;
      const workspace = data.workspace_default ? normalizeLayout(data.workspace_default) : null;
      if (gen !== editGen) {
        // An edit landed while the read was in flight: keep it, learn the default.
        emit({ status: "ready", workspace });
        return;
      }
      emit({ status: "ready", own, workspace, ...effective(own, workspace) });
    } catch {
      if (gen !== editGen) {
        emit({ status: "unavailable" });
        return;
      }
      // No server answer: the default rail, never a stale cached one that
      // might point at things this account can no longer open.
      emit({ status: "unavailable", own: null, workspace: null, layout: BUILTIN_LAYOUT, source: "builtin" });
    }
  })();
  return loadPromise;
}

/**
 * loadOwnSidebar reads the layouts again when the last read failed (the
 * once-per-page read is otherwise shared) and resolves to the state, whose
 * `status` says whether the person's own layout is really known.
 */
export async function loadOwnSidebar(): Promise<SidebarState> {
  if (state.status === "unavailable") loadPromise = null;
  await loadSidebar();
  return state;
}

/** sidebarSettled resolves once the read has landed, or after `timeoutMs`. */
export function sidebarSettled(timeoutMs: number): Promise<SidebarState> {
  return Promise.race([
    loadSidebar().then(() => state),
    new Promise<SidebarState>((resolve) => setTimeout(() => resolve(state), timeoutMs)),
  ]);
}

// Every edit is numbered (rev), increasing, and each save carries the number
// of the layout it sends. The server applies a save only when its number is
// newer than the stored one, so two requests that overlap — a save still in
// flight when the page unloads and sends the next one with keepalive — can
// never leave the older layout on top, whichever reaches it last.
//
// The numbers continue the server's own sequence (the stored rev, read with
// the layout and returned by every save), never a device clock: a device
// whose clock runs ahead cannot lock out another. If another tab or device
// saved meanwhile, the server answers "stale" with what it stores, and that
// is what this page then shows (the other edit won; nothing claims "saved").
let lastRev = 0;
let pendingRev = 0;
function learnRev(rev: unknown) {
  if (typeof rev === "number" && Number.isFinite(rev) && rev > lastRev) lastRev = rev;
}
function nextRev(): number {
  lastRev += 1;
  return lastRev;
}

function sendSave(keepalive: boolean): Promise<Response> {
  const own = state.own;
  const rev = pendingRev;
  return fetch("/api/sidebar", {
    method: own ? "PUT" : "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(own ? { ...own, rev } : { rev }),
    keepalive,
  });
}

// Saves run one at a time, in order: each waits for the one before it to
// finish, and sends the layout as it is when its turn comes. So an older
// request can never reach the server after a newer one and overwrite it, and
// a save whose edit has already been superseded is skipped (the newer one
// carries it).
let saveChain: Promise<void> = Promise.resolve();

function flushSave(gen: number): Promise<void> {
  saveChain = saveChain.then(() => sendInTurn(gen));
  return saveChain;
}

// The newest edit the server has confirmed. An edit is "unsaved" until its
// generation is confirmed here, whether its debounce is still waiting, its
// request is queued behind a slow one, or the request is in flight; leaving
// the page sends any unsaved edit with keepalive (flushOnHide).
let confirmedGen = 0;

async function sendInTurn(gen: number) {
  if (gen !== editGen) return;
  try {
    await handleSaveResponse(await sendSave(false), gen, state.own);
  } catch {
    if (gen === editGen) emit({ saving: false, saveError: true });
  }
}

// handleSaveResponse applies a save's answer, from an ordinary save or from
// the keepalive one sent on pagehide (whose answer arrives if the page was
// kept in the back-forward cache and restored).
async function handleSaveResponse(res: Response, gen: number, own: SidebarLayout | null) {
  if (!res.ok) throw new Error(String(res.status));
  const data = (await res.json().catch(() => ({}))) as { layout?: unknown; stale?: unknown; rev?: unknown };
  learnRev(data.rev);
  if (gen !== editGen) return; // a newer edit has its own save queued
  confirmedGen = gen;
  const saved = data.layout ? normalizeLayout(data.layout) : null;
  if (data.stale === true) {
    // Overtaken by a newer save from another tab or device: show what the
    // server keeps, which is what every page will load from now on.
    emit({ own: saved, saving: false, saveError: false, ...effective(saved, state.workspace) });
    return;
  }
  if (own && saved && !sameLayout(saved, own)) {
    // The server repaired something (a name cut to length, say): show its copy.
    emit({ own: saved, layout: saved, saving: false, saveError: false });
  } else {
    emit({ saving: false, saveError: false });
  }
}

function scheduleSave() {
  const gen = ++editGen;
  pendingRev = nextRev();
  if (saveTimer) clearTimeout(saveTimer);
  emit({ saving: true });
  saveTimer = setTimeout(() => {
    saveTimer = null;
    void flushSave(gen);
  }, SAVE_DELAY_MS);
}

/** setSidebarLayout makes `next` the person's own layout (shown now, saved shortly). */
export function setSidebarLayout(next: SidebarLayout) {
  const layout = normalizeLayout(next);
  emit({ own: layout, layout, source: "user", saveError: false });
  scheduleSave();
}

/** updateSidebarLayout applies an edit to the current effective layout. */
export function updateSidebarLayout(edit: (current: SidebarLayout) => SidebarLayout) {
  setSidebarLayout(edit(state.layout));
}

/** resetSidebarLayout drops the person's own layout: they follow the default again. */
export function resetSidebarLayout() {
  emit({ own: null, saveError: false, ...effective(null, state.workspace) });
  scheduleSave();
}

/** retrySidebarSave re-sends the current layout after a failed save. */
export function retrySidebarSave() {
  emit({ saveError: false });
  scheduleSave();
}

// An edit still waiting out the debounce when the page goes away (a reload,
// a closed tab, a shortcut that loads another page) is sent at once, with
// keepalive so the browser finishes the request after the page is gone.
function flushOnHide() {
  if (saveTimer) {
    clearTimeout(saveTimer);
    saveTimer = null;
  }
  // Nothing unsaved: the newest edit is already confirmed (or there was none).
  if (editGen === 0 || confirmedGen === editGen) return;
  // Send the newest edit now, whether it was still waiting, queued behind a
  // slow save or in flight: revisions make a duplicate harmless (the server
  // answers the older or equal one as stale). If the page is kept in the
  // back-forward cache, the answer is handled like any other on return.
  const gen = editGen;
  const own = state.own;
  void sendSave(true)
    .then((res) => handleSaveResponse(res, gen, own))
    .catch(() => {
      if (gen === editGen) emit({ saving: false, saveError: true });
    });
}
if (typeof window !== "undefined") window.addEventListener("pagehide", flushOnHide);

/** Test hook: what the page does when it is hidden for good. */
export function flushSidebarSaveForTests() {
  flushOnHide();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  void loadSidebar();
  return () => {
    listeners.delete(listener);
  };
}

function snapshot() {
  return state;
}

/** useSidebar: the shared layout state, re-rendering on every change. */
export function useSidebar(): SidebarState {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

export function getSidebarState(): SidebarState {
  return state;
}

/** Test hook: forget everything (the next read starts over). */
export function resetSidebarStoreForTests() {
  if (saveTimer) clearTimeout(saveTimer);
  saveTimer = null;
  saveChain = Promise.resolve();
  confirmedGen = 0;
  lastRev = 0;
  pendingRev = 0;
  loadPromise = null;
  editGen = 0;
  listeners.clear();
  state = initialState();
}

// ── The Customize panel's open state (one panel per page, opened from the
// rail, the account menu, or a chat's or project's menu). ──────────────────

let customizerOpen = false;
const customizerListeners = new Set<() => void>();

export function setSidebarCustomizerOpen(open: boolean) {
  if (customizerOpen === open) return;
  customizerOpen = open;
  for (const l of customizerListeners) l();
}

export function openSidebarCustomizer() {
  setSidebarCustomizerOpen(true);
}

function subscribeCustomizer(listener: () => void) {
  customizerListeners.add(listener);
  return () => {
    customizerListeners.delete(listener);
  };
}

export function useSidebarCustomizerOpen(): boolean {
  return useSyncExternalStore(
    subscribeCustomizer,
    () => customizerOpen,
    () => false,
  );
}
