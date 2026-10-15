import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";

// public/sw.js is served as-is, not bundled, so these tests evaluate the real
// file against a fake ServiceWorkerGlobalScope and drive its listeners. Every
// window action (focus, navigate, open) is recorded in order, which is what
// the click contract is about: the browser grants one focus-or-open per click.

const SOURCE = readFileSync(resolve(__dirname, "../../public/sw.js"), "utf8");
const ORIGIN = "https://fleet.example.com";
const CHROME_UA =
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36";
const FIREFOX_UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:140.0) Gecko/20100101 Firefox/140.0";
const SAFARI_UA =
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15";
// Chrome on iOS runs on WebKit, like every iOS browser.
const CHROME_IOS_UA =
  "Mozilla/5.0 (iPhone; CPU iPhone OS 26_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0.0.0 Mobile/15E148 Safari/604.1";

type FakeWindow = {
  url: string;
  controlled: boolean;
  focusFails?: boolean;
  navigateFails?: boolean;
  // A frame inside a page (an email preview's srcdoc frame, say) rather than a tab.
  nested?: boolean;
  id?: string;
};
type Listener = (event: unknown) => void;
// What clients.openWindow does: Chrome and Firefox open the window and return
// it; Safari (26.5 on macOS) returns null and opens nothing; Chromium also
// returns null for a window that opened on an error page; WebKit is reported to
// return null before a window it did open shows up ("late", here one that was
// redirected on the way).
type OpenWindow = "opens" | "null" | "late" | "rejects";

function loadWorker(windows: FakeWindow[], openWindow: OpenWindow = "opens", userAgent = CHROME_UA) {
  const actions: string[] = [];
  let seq = 0;
  const listeners: Record<string, Listener> = {};
  const client = (w: FakeWindow) => ({
    id: (w.id ??= `client-${++seq}`),
    url: w.url,
    frameType: w.nested ? "nested" : "top-level",
    // A real focus() rejects when the window does not end up focused or the
    // click's window-action grant is spent.
    focus: vi.fn(async () => {
      actions.push(`focus ${w.url}`);
      if (w.focusFails) throw new TypeError("not focused");
      return client(w);
    }),
    navigate: vi.fn(async (url: string) => {
      actions.push(`navigate ${w.url} -> ${url}`);
      if (w.navigateFails) throw new TypeError("cannot navigate");
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
      if (openWindow === "rejects") throw new TypeError("not allowed to open a window");
      if (openWindow === "null") return null;
      // A window the worker opens is one it controls, most recently focused.
      const opened = { url: openWindow === "late" ? `${ORIGIN}/orchestrator?task=t-1` : url, controlled: true };
      windows.unshift(opened);
      return openWindow === "late" ? null : client(opened);
    }),
    claim: vi.fn(async () => {}),
  };
  const scope = {
    location: new URL(`${ORIGIN}/sw.js`),
    navigator: { userAgent },
    clients,
    skipWaiting: vi.fn(),
    registration: { showNotification: vi.fn(async () => {}) },
    addEventListener: (type: string, fn: Listener) => {
      listeners[type] = fn;
    },
  };
  // In a real worker scope `clients`, `location`, `registration` and
  // `navigator` are also bare globals; shadow them so jsdom's own never stand in.
  new Function("self", "clients", "location", "registration", "navigator", SOURCE)(
    scope,
    clients,
    scope.location,
    scope.registration,
    scope.navigator,
  );
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

  it("never takes over a tab in another part of the app outside WebKit", async () => {
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

  it("tries no second window action when the tab cannot be navigated", async () => {
    // The click's one window action went on the focus; a fallback open would
    // be refused, so the failure surfaces instead of being retried.
    const worker = loadWorker([{ url: `${ORIGIN}/orchestrator`, controlled: true, navigateFails: true }]);
    await expect(click(worker, `${ORIGIN}/orchestrator/tasks/t-1`)).rejects.toThrow("cannot navigate");
    expect(worker.actions).toEqual([
      `focus ${ORIGIN}/orchestrator`,
      `navigate ${ORIGIN}/orchestrator -> ${ORIGIN}/orchestrator/tasks/t-1`,
    ]);
  });

  it("counts only tabs, not frames inside a page", async () => {
    const framed = loadWorker([{ url: `${ORIGIN}/orchestrator/frame`, controlled: true, nested: true }]);
    await click(framed, `${ORIGIN}/orchestrator/tasks/t-1`);
    expect(framed.actions).toEqual([`open ${ORIGIN}/orchestrator/tasks/t-1`]);

    const root = loadWorker([
      { url: "about:srcdoc", controlled: true, nested: true },
      { url: `${ORIGIN}/settings`, controlled: true },
    ]);
    await click(root, ORIGIN);
    expect(root.actions).toEqual([`focus ${ORIGIN}/settings`]);
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

    // The query and the fragment come along.
    const chat = loadWorker([{ url: `${ORIGIN}/chat`, controlled: true }]);
    await click(chat, "https://fleet.public.example/chat?c=abc#latest");
    expect(chat.actions).toEqual([`focus ${ORIGIN}/chat`, `navigate ${ORIGIN}/chat -> ${ORIGIN}/chat?c=abc#latest`]);

    const root = loadWorker([{ url: `${ORIGIN}/chat`, controlled: true }]);
    await click(root, "https://fleet.public.example");
    expect(root.actions).toEqual([`focus ${ORIGIN}/chat`]);
  });

  it("keeps a path that begins with two slashes on this origin", async () => {
    // Rebuilt as a string, "//evil.example/x" would read as a link to another
    // host; the path must stay a path.
    for (const [link, path] of [
      [`${ORIGIN}//evil.example/x`, "//evil.example/x"],
      ["https://fleet.public.example//evil.example/x", "//evil.example/x"],
      ["/a/..//evil.example", "//evil.example"],
      [`${ORIGIN}/a/..//evil.example/chat?c=abc#latest`, "//evil.example/chat?c=abc#latest"],
      ["//evil.example/x", "/x"],
    ]) {
      const worker = loadWorker([]);
      await click(worker, link);
      expect(worker.actions).toEqual([`open ${ORIGIN}${path}`]);
      expect(new URL(worker.clients.openWindow.mock.calls[0][0]).origin).toBe(ORIGIN);
    }
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
    // The claim is handed to waitUntil, so activation waits for it.
    expect(claimed).toBe(worker.clients.claim.mock.results[0].value);
    await claimed;
  });
});

describe("sw.js notification click where the browser opens no window", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const TASK = `${ORIGIN}/orchestrator/tasks/t-1`;

  // Clicks with fake timers: the WebKit fallback waits for a late window first.
  async function clickAndWait(worker: ReturnType<typeof loadWorker>, url: string) {
    vi.useFakeTimers();
    const done = click(worker, url);
    await vi.advanceTimersByTimeAsync(1000);
    return done;
  }

  it("sends the most recently used tab to the link in Safari, which opens no window", async () => {
    const worker = loadWorker(
      [
        { url: `${ORIGIN}/settings`, controlled: true },
        { url: `${ORIGIN}/chat`, controlled: true },
      ],
      "null",
      SAFARI_UA,
    );
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/settings`, `navigate ${ORIGIN}/settings -> ${TASK}`]);
  });

  it("does the same in any iOS browser, all of which run on WebKit", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: true }], "null", CHROME_IOS_UA);
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/settings`, `navigate ${ORIGIN}/settings -> ${TASK}`]);
  });

  it("waits a second for a late window before reusing a tab", async () => {
    vi.useFakeTimers();
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: true }], "null", SAFARI_UA);
    const done = click(worker, TASK);
    await vi.advanceTimersByTimeAsync(999);
    expect(worker.actions).toEqual([`open ${TASK}`]);
    await vi.advanceTimersByTimeAsync(1);
    await done;
    expect(worker.actions).toHaveLength(3);
  });

  it("brings forward a window that opened late, wherever it was redirected, and reloads no tab", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: true }], "late", SAFARI_UA);
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/orchestrator?task=t-1`]);
  });

  it("does not take a window that was already open for a late one", async () => {
    // An uncontrolled tab at the very same link (a hard reload) was there
    // before the click, so the click still reuses the controlled tab.
    const worker = loadWorker(
      [
        { url: `${ORIGIN}/settings`, controlled: true },
        { url: TASK, controlled: false },
      ],
      "null",
      SAFARI_UA,
    );
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/settings`, `navigate ${ORIGIN}/settings -> ${TASK}`]);
  });

  it("falls back the same way when opening a window is refused outright", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/chat?c=draft`, controlled: true }], "rejects", SAFARI_UA);
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/chat?c=draft`, `navigate ${ORIGIN}/chat?c=draft -> ${TASK}`]);
  });

  it("still navigates the reused tab when focusing it fails", async () => {
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: true, focusFails: true }], "null", SAFARI_UA);
    await clickAndWait(worker, TASK);
    expect(worker.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/settings`, `navigate ${ORIGIN}/settings -> ${TASK}`]);
  });

  it("surfaces a failed navigation of the reused tab", async () => {
    vi.useFakeTimers();
    const worker = loadWorker([{ url: `${ORIGIN}/settings`, controlled: true, navigateFails: true }], "null", SAFARI_UA);
    const done = click(worker, TASK);
    const settled = expect(done).rejects.toThrow("cannot navigate");
    await vi.advanceTimersByTimeAsync(1000);
    await settled;
  });

  it("brings forward a tab it cannot navigate, and does nothing more with no tab", async () => {
    const uncontrolled = loadWorker([{ url: `${ORIGIN}/settings`, controlled: false }], "null", SAFARI_UA);
    await clickAndWait(uncontrolled, TASK);
    expect(uncontrolled.actions).toEqual([`open ${TASK}`, `focus ${ORIGIN}/settings`]);
    vi.useRealTimers();

    for (const windows of [[], [{ url: `${ORIGIN}/settings/frame`, controlled: true, nested: true }]]) {
      const worker = loadWorker(windows, "null", SAFARI_UA);
      await clickAndWait(worker, TASK);
      expect(worker.actions).toEqual([`open ${TASK}`]);
      vi.useRealTimers();
    }
  });

  it("neither waits nor reuses a tab outside WebKit, even when openWindow answers null", async () => {
    // Chromium answers null for a window it did open when that window shows an
    // error page (fleet unreachable): reusing a tab would reload it for nothing.
    for (const [ua, mode] of [
      [CHROME_UA, "null"],
      [CHROME_UA, "opens"],
      [FIREFOX_UA, "null"],
    ] as const) {
      vi.useFakeTimers();
      const worker = loadWorker([{ url: `${ORIGIN}/chat?c=draft`, controlled: true }], mode, ua);
      const done = click(worker, TASK);
      await vi.advanceTimersByTimeAsync(0);
      expect(vi.getTimerCount(), `${mode} ${ua}`).toBe(0);
      await done;
      expect(worker.actions, `${mode} ${ua}`).toEqual([`open ${TASK}`]);
      vi.useRealTimers();
    }
  });
});

