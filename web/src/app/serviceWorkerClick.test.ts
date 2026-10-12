import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it, vi } from "vitest";

// public/sw.js is served as-is, not bundled, so these tests evaluate the real
// file against a fake ServiceWorkerGlobalScope and drive its listeners. Every
// window action (focus, navigate, open) is recorded in order, which is what
// the click contract is about: the browser grants one focus-or-open per click.

const SOURCE = readFileSync(resolve(__dirname, "../../public/sw.js"), "utf8");
const ORIGIN = "https://fleet.example.com";

type FakeWindow = { url: string; controlled: boolean; focusFails?: boolean };
type Listener = (event: unknown) => void;

function loadWorker(windows: FakeWindow[]) {
  const actions: string[] = [];
  const listeners: Record<string, Listener> = {};
  const client = (w: FakeWindow) => ({
    url: w.url,
    // A real focus() rejects when the window does not end up focused or the
    // click's window-action grant is spent.
    focus: vi.fn(async () => {
      actions.push(`focus ${w.url}`);
      if (w.focusFails) throw new TypeError("not focused");
      return client(w);
    }),
    navigate: vi.fn(async (url: string) => {
      actions.push(`navigate ${w.url} -> ${url}`);
      return client({ ...w, url });
    }),
  });
  const clients = {
    // Window clients come back most recently focused first, as in the browser.
    matchAll: vi.fn(async (opts?: { includeUncontrolled?: boolean }) =>
      windows.filter((w) => opts?.includeUncontrolled || w.controlled).map(client),
    ),
    openWindow: vi.fn(async (url: string) => {
      actions.push(`open ${url}`);
      return null;
    }),
    claim: vi.fn(async () => {}),
  };
  const scope = {
    location: new URL(`${ORIGIN}/sw.js`),
    clients,
    skipWaiting: vi.fn(),
    registration: { showNotification: vi.fn(async () => {}) },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
  };
  // `clients` is also a global in a real worker scope.
  new Function("self", "clients", SOURCE)(scope, clients);
  return { actions, listeners, clients, scope };
}

async function click(worker: ReturnType<typeof loadWorker>, url: string | undefined) {
  let settled: Promise<unknown> = Promise.resolve();
  const notification = { data: url === undefined ? {} : { url }, close: vi.fn() };
  worker.listeners.notificationclick({
    notification,
    waitUntil: (p: Promise<unknown>) => {
      settled = p;
    },
  });
  await settled;
  return notification;
}

describe("sw.js notification click", () => {
  it("takes an open Operations Center tab to a task's record", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/orchestrator`, controlled: true }]);
    const notification = await click(worker, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(notification.close).toHaveBeenCalled();
    expect(worker.actions).toEqual([
      `focus ${ORIGIN}/orchestrator`,
      `navigate ${ORIGIN}/orchestrator -> ${ORIGIN}/orchestrator/tasks/t-1`,
    ]);
  });

  it("never takes over a tab in another part of the app", async () => {
    // Navigating reloads the page: an unsent chat message would be lost.
    const worker = loadWorker([
      { url: `${ORIGIN}/chat?c=draft`, controlled: true },
      { url: `${ORIGIN}/settings`, controlled: true },
    ]);
    await click(worker, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(worker.actions).toEqual([`open ${ORIGIN}/orchestrator/tasks/t-1`]);
  });

  it("resolves a relative chat link against the app and uses the most recent chat tab", async () => {
    const worker = loadWorker([
      { url: `${ORIGIN}/orchestrator`, controlled: true },
      { url: `${ORIGIN}/chat`, controlled: true },
      { url: `${ORIGIN}/chat?c=older`, controlled: true },
    ]);
    await click(worker, "/chat?c=abc");
    expect(worker.actions).toEqual([
      `focus ${ORIGIN}/chat`,
      `navigate ${ORIGIN}/chat -> ${ORIGIN}/chat?c=abc`,
    ]);
  });

  it("still navigates the tab when focusing it fails", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/orchestrator`, controlled: true, focusFails: true }]);
    await click(worker, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(worker.actions).toEqual([
      `focus ${ORIGIN}/orchestrator`,
      `navigate ${ORIGIN}/orchestrator -> ${ORIGIN}/orchestrator/tasks/t-1`,
    ]);
  });

  it("opens the link when no tab is open", async () => {
    const worker = loadWorker([]);
    await click(worker, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(worker.actions).toEqual([`open ${ORIGIN}/orchestrator/tasks/t-1`]);
  });

  it("opens the link in a new window when the matching tab cannot be navigated", async () => {
    // An uncontrolled tab (a hard reload bypasses the worker) cannot be
    // navigated, and focusing it first would spend the click's one window
    // action, leaving nothing to open the link with.
    const worker = loadWorker([{ url: `${ORIGIN}/orchestrator`, controlled: false }]);
    await click(worker, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(worker.actions).toEqual([`open ${ORIGIN}/orchestrator/tasks/t-1`]);
  });

  it("brings the app forward without navigating for a link to its root", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: false }]);
    await click(worker, ORIGIN);
    expect(worker.actions).toEqual([`focus ${ORIGIN}/settings`]);

    const none = loadWorker([]);
    await click(none, undefined);
    expect(none.actions).toEqual([`open ${ORIGIN}/`]);
  });

  it("moves a link that names another host onto the origin this browser uses", async () => {
    // FLEET_PUBLIC_URL may name another host; the session cookie lives here.
    const worker = loadWorker([{ url: `${ORIGIN}/orchestrator`, controlled: true }]);
    await click(worker, "https://fleet.public.example/orchestrator/tasks/t-1");
    expect(worker.actions).toEqual([
      `focus ${ORIGIN}/orchestrator`,
      `navigate ${ORIGIN}/orchestrator -> ${ORIGIN}/orchestrator/tasks/t-1`,
    ]);

    const root = loadWorker([{ url: `${ORIGIN}/chat`, controlled: true }]);
    await click(root, "https://fleet.public.example");
    expect(root.actions).toEqual([`focus ${ORIGIN}/chat`]);
  });
});

describe("sw.js lifecycle", () => {
  it("takes effect at once and takes control of the Fleet tabs already open", async () => {
    const worker = loadWorker([]);
    worker.listeners.install({});
    expect(worker.scope.skipWaiting).toHaveBeenCalled();
    let claimed: Promise<unknown> | undefined;
    worker.listeners.activate({
      waitUntil: (p: Promise<unknown>) => {
        claimed = p;
      },
    });
    await claimed;
    expect(worker.clients.claim).toHaveBeenCalled();
  });
});
