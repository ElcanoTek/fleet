import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

// After the operator rotates the VAPID keys, the switch must not read "on" for
// a subscription the push service now refuses. These tests run the row
// against a fake browser whose subscription records the key it was made with.

const keyBytes = (fill: number) => {
  const k = new Uint8Array(65).fill(fill);
  k[0] = 4;
  return k;
};
const b64url = (bytes: Uint8Array) =>
  Buffer.from(bytes).toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const OLD_KEY = keyBytes(1);
const NEW_KEY = keyBytes(2);

type FakeSub = ReturnType<typeof makeSub>;
function makeSub(endpoint: string, key: Uint8Array, browser: { current: FakeSub | null; events: string[] }) {
  const sub = {
    endpoint,
    options: { applicationServerKey: key.buffer },
    unsubscribe: vi.fn(async () => {
      browser.events.push(`unsubscribe ${endpoint}`);
      if (browser.current === sub) browser.current = null;
      return true;
    }),
    toJSON: () => ({ endpoint, keys: { auth: "auth", p256dh: "p256dh" } }),
  };
  return sub;
}

// stubBrowser keeps one "current" subscription the way a browser does:
// unsubscribe clears it, subscribe replaces it. The server side keeps the
// signed-in user's stored endpoints, which /api/push/status reads.
function stubBrowser(opts: { start: "old" | "none"; renewed?: boolean | number }) {
  const browser = { current: null as FakeSub | null, events: [] as string[] };
  const old = makeSub("https://relay.example/old", OLD_KEY, browser);
  if (opts.start === "old") browser.current = old;
  const stored = new Set<string>(opts.start === "old" ? [old.endpoint] : []);
  // Whether the browser-wide push lock was held when each request was made
  // (only meaningful when a test installs stubLocks).
  const lockedAt: Record<string, boolean> = {};
  const reg = {
    pushManager: {
      getSubscription: async () => browser.current,
      subscribe: vi.fn(async (o: { applicationServerKey: ArrayBuffer }) => {
        browser.events.push(`subscribe ${b64url(new Uint8Array(o.applicationServerKey))}`);
        browser.current = makeSub("https://relay.example/new", new Uint8Array(o.applicationServerKey), browser);
        return browser.current;
      }),
    },
  };
  vi.stubGlobal("PushManager", function PushManager() {});
  vi.stubGlobal(
    "Notification",
    Object.assign(function Notification() {}, {
      permission: "granted",
      requestPermission: vi.fn(async () => "granted"),
    }),
  );
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: {
      getRegistration: vi.fn(async () => reg),
      register: vi.fn(async () => reg),
      ready: Promise.resolve(reg),
    },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url !== "/api/push/vapid-public-key") {
        browser.events.push(`${init?.method ?? "GET"} ${url}`);
        lockedAt[url] = locks.holding;
      }
      if (url === "/api/push/vapid-public-key") return Response.json({ key: b64url(NEW_KEY) });
      const body = JSON.parse((init?.body as string | undefined) ?? "{}") as {
        endpoint?: string;
        old_endpoint?: string;
      };
      if (url === "/api/push/status") return Response.json({ subscribed: stored.has(body.endpoint ?? "") });
      if (url === "/api/push/renew") {
        const renewed = opts.renewed ?? true;
        if (typeof renewed === "number") return new Response("boom", { status: renewed });
        if (renewed) {
          stored.delete(body.old_endpoint ?? "");
          stored.add(body.endpoint ?? "");
        }
        return Response.json({ renewed });
      }
      if (url === "/api/push/subscribe") stored.add(body.endpoint ?? "");
      if (url === "/api/push/unsubscribe") stored.delete(body.endpoint ?? "");
      return new Response(null, { status: 204 });
    }),
  );
  return { browser, old, stored, lockedAt };
}

// stubLocks installs a Web Locks API that grants one holder at a time and
// records whether it is held, so a test can see which requests ran under it.
const locks = { holding: false };
function stubLocks() {
  let queue: Promise<unknown> = Promise.resolve();
  Object.defineProperty(navigator, "locks", {
    configurable: true,
    value: {
      request: (_name: string, a: unknown, b?: () => Promise<unknown>) => {
        const fn = (b ?? a) as () => Promise<unknown>;
        const run = queue.then(async () => {
          locks.holding = true;
          try {
            return await fn();
          } finally {
            locks.holding = false;
          }
        });
        queue = run.catch(() => undefined);
        return run;
      },
    },
  });
}

// A fresh module graph per test: the key check is memoized per page load.
async function renderRow() {
  vi.resetModules();
  const { BrowserNotificationsRow } = await import("./BrowserNotificationsRow");
  render(<BrowserNotificationsRow />);
  const sw = screen.getByTestId("browser-notifications-switch");
  await waitFor(() => expect(sw).not.toBeDisabled());
  return sw;
}

describe("BrowserNotificationsRow after a VAPID key rotation", () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
    Reflect.deleteProperty(navigator, "locks");
    window.localStorage.clear();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("renews a subscription made with the earlier key, then reads on", async () => {
    const { browser } = stubBrowser({ start: "old", renewed: true });
    const sw = await renderRow();
    expect(sw).toHaveAttribute("aria-checked", "true");
    expect(browser.events).toEqual([
      "unsubscribe https://relay.example/old",
      `subscribe ${b64url(NEW_KEY)}`,
      "POST /api/push/renew",
      // The switch then asks whether the server holds the renewed one.
      "POST /api/push/status",
    ]);
    expect(browser.current?.endpoint).toBe("https://relay.example/new");
  });

  it("reads off when the old subscription was not this user's to renew", async () => {
    const { browser } = stubBrowser({ start: "old", renewed: false });
    const sw = await renderRow();
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(browser.current).toBeNull();
    // The renewal itself let the new subscription go; nothing was left for
    // the switch's own check to ask about.
    expect(browser.events).toEqual([
      "unsubscribe https://relay.example/old",
      `subscribe ${b64url(NEW_KEY)}`,
      "POST /api/push/renew",
      "unsubscribe https://relay.example/new",
    ]);
  });

  it("keeps a renewal the server has not confirmed, and says so, instead of letting it go", async () => {
    // The move did not land, so the server holds no row for the new
    // subscription; dropping it would lose the retry on the next load.
    const { browser } = stubBrowser({ start: "old", renewed: 503 });
    const sw = await renderRow();
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(browser.current?.endpoint).toBe("https://relay.example/new");
    expect(screen.getByText(/could not be renewed after a key change/)).toBeInTheDocument();
    expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBe("https://relay.example/old");
  });

  it("checks with the server and stores a new subscription only while holding the push lock", async () => {
    stubLocks();
    const loaded = stubBrowser({ start: "old", renewed: true });
    const sw = await renderRow();
    expect(sw).toHaveAttribute("aria-checked", "true");
    expect(loaded.lockedAt).toEqual({ "/api/push/renew": true, "/api/push/status": true });

    // Turning it off and on again stores the subscription under the lock too.
    fireEvent.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute("aria-checked", "false"));
    fireEvent.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute("aria-checked", "true"));
    expect(loaded.lockedAt["/api/push/subscribe"]).toBe(true);
  });

  it("turning it on replaces a subscription made with the earlier key", async () => {
    // Off at load (no subscription), then one made with the earlier key turns
    // up (another tab, an older install) before the user turns it on.
    const { browser, old } = stubBrowser({ start: "none" });
    const sw = await renderRow();
    expect(sw).toHaveAttribute("aria-checked", "false");
    browser.current = old;
    fireEvent.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute("aria-checked", "true"));
    expect(browser.events).toEqual([
      "DELETE /api/push/unsubscribe",
      "unsubscribe https://relay.example/old",
      `subscribe ${b64url(NEW_KEY)}`,
      "POST /api/push/subscribe",
    ]);
  });
});
