import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// After the operator rotates the VAPID keys, a subscription made with the old
// key is refused by the push service for every send. These tests drive the
// once-per-page check against a fake browser: a subscription that records the
// key it was made with, and the server's current key.

const keyBytes = (fill: number) => {
  const k = new Uint8Array(65).fill(fill);
  k[0] = 4;
  return k;
};
const b64url = (bytes: Uint8Array) =>
  Buffer.from(bytes).toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const OLD_KEY = keyBytes(1);
const NEW_KEY = keyBytes(2);

type Browser = { current: ReturnType<typeof fakeSub> | null; events: string[] };

function fakeSub(endpoint: string, key: Uint8Array, browser: Browser) {
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

// stubBrowser keeps one current subscription the way a browser does:
// unsubscribe clears it, subscribe replaces it, and subscribing with another
// key while one exists throws, as PushManager.subscribe does.
function stubBrowser(opts: {
  permission?: NotificationPermission;
  subKey?: Uint8Array | null;
  serverKey?: Uint8Array;
  vapidStatus?: number;
  renew?: { renewed: boolean } | "network-error" | number;
  subscribeFails?: boolean;
}) {
  const browser: Browser = { current: null, events: [] };
  const events = browser.events;
  const old = opts.subKey === null ? null : fakeSub("https://relay.example/old", opts.subKey ?? OLD_KEY, browser);
  browser.current = old;
  let made = 0;
  const subscribe = vi.fn(async (o: { applicationServerKey: ArrayBuffer }) => {
    const key = new Uint8Array(o.applicationServerKey);
    events.push(`subscribe ${b64url(key)}`);
    if (opts.subscribeFails) throw new DOMException("push service error", "AbortError");
    const held = browser.current?.options.applicationServerKey;
    if (held && !Buffer.from(new Uint8Array(held)).equals(Buffer.from(key))) {
      throw new DOMException("a subscription with another key exists", "InvalidStateError");
    }
    browser.current ??= fakeSub(`https://relay.example/new${made++ || ""}`, key, browser);
    return browser.current;
  });
  vi.stubGlobal("PushManager", function PushManager() {});
  vi.stubGlobal(
    "Notification",
    Object.assign(function Notification() {}, { permission: opts.permission ?? "granted" }),
  );
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: {
      getRegistration: vi.fn(async () => ({
        pushManager: { getSubscription: async () => browser.current, subscribe },
      })),
    },
  });
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    events.push(`${init?.method ?? "GET"} ${url}${init?.body ? ` ${String(init.body)}` : ""}`);
    if (url === "/api/push/vapid-public-key") {
      return opts.vapidStatus
        ? new Response(null, { status: opts.vapidStatus })
        : Response.json({ key: b64url(opts.serverKey ?? NEW_KEY) });
    }
    if (url === "/api/push/renew") {
      if (opts.renew === "network-error") throw new TypeError("network down");
      if (typeof opts.renew === "number") return new Response("boom", { status: opts.renew });
      return Response.json(opts.renew ?? { renewed: true });
    }
    return new Response(null, { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return { events, browser, old, subscribe, fetchMock };
}

// A fresh module per test: the check shares a running promise per module.
async function loadCheck() {
  vi.resetModules();
  return (await import("./pushKey")).ensureCurrentPushKey;
}

const renewBody = (oldEndpoint: string, endpoint: string) =>
  `POST /api/push/renew ${JSON.stringify({
    old_endpoint: oldEndpoint,
    endpoint,
    keys: { auth: "auth", p256dh: "p256dh" },
  })}`;

describe("ensureCurrentPushKey", () => {
  beforeEach(() => {
    vi.resetModules();
    window.localStorage.clear();
  });
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
    Reflect.deleteProperty(navigator, "locks");
    window.localStorage.clear();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("leaves a subscription on the current key alone", async () => {
    const b = stubBrowser({ subKey: NEW_KEY, serverKey: NEW_KEY });
    expect(await (await loadCheck())()).toBe("current");
    expect(b.events).toEqual(["GET /api/push/vapid-public-key"]);
  });

  it("renews a subscription made with an earlier key and moves this user's row to it", async () => {
    const b = stubBrowser({ renew: { renewed: true } });
    expect(await (await loadCheck())()).toBe("renewed");
    expect(b.events).toEqual([
      "GET /api/push/vapid-public-key",
      // The old one must go before the browser accepts a new key.
      "unsubscribe https://relay.example/old",
      `subscribe ${b64url(NEW_KEY)}`,
      renewBody("https://relay.example/old", "https://relay.example/new"),
    ]);
    expect(b.browser.current?.endpoint).toBe("https://relay.example/new");
  });

  it("lets the new subscription go when the old one was not this user's", async () => {
    // Another account's subscription on a shared browser: renewing must not
    // sign up whoever is signed in now.
    const b = stubBrowser({ renew: { renewed: false } });
    expect(await (await loadCheck())()).toBe("dropped");
    expect(b.browser.current).toBeNull();
  });

  it("keeps the new subscription when the server gives no answer, and retries the move next time", async () => {
    for (const failure of ["network-error", 503] as const) {
      window.localStorage.clear();
      const b = stubBrowser({ renew: failure });
      expect(await (await loadCheck())()).toBe("pending");
      expect(b.browser.current?.endpoint).toBe("https://relay.example/new");

      // Next load: the subscription is on the current key, but the move the
      // server never confirmed is retried from the remembered old endpoint.
      b.events.length = 0;
      const again = stubBrowser({ subKey: NEW_KEY, renew: { renewed: true } });
      again.browser.current = b.browser.current;
      expect(await (await loadCheck())()).toBe("renewed");
      expect(again.events).toEqual([
        "GET /api/push/vapid-public-key",
        renewBody("https://relay.example/old", "https://relay.example/new"),
      ]);
      expect(await (await loadCheck())()).toBe("current");
      vi.unstubAllGlobals();
    }
  });

  it("reports a browser that will not subscribe again instead of hiding it", async () => {
    const b = stubBrowser({ subscribeFails: true });
    expect(await (await loadCheck())()).toBe("failed");
    expect(b.fetchMock.mock.calls.some(([url]) => url === "/api/push/renew")).toBe(false);
  });

  it("changes nothing when signed out (no key to compare against)", async () => {
    const b = stubBrowser({ vapidStatus: 401 });
    expect(await (await loadCheck())()).toBe("none");
    expect(b.old!.unsubscribe).not.toHaveBeenCalled();
    expect(b.subscribe).not.toHaveBeenCalled();
  });

  it("asks nothing without notification permission or a subscription", async () => {
    const noPermission = stubBrowser({ permission: "default" });
    expect(await (await loadCheck())()).toBe("none");
    expect(noPermission.fetchMock).not.toHaveBeenCalled();

    vi.unstubAllGlobals();
    const noSub = stubBrowser({ subKey: null });
    expect(await (await loadCheck())()).toBe("none");
    expect(noSub.fetchMock).not.toHaveBeenCalled();
  });

  it("shares a running check, and checks again once it has finished", async () => {
    // A tab left open across a rotation must notice on its next check.
    const b = stubBrowser({ renew: { renewed: true } });
    const check = await loadCheck();
    const [first, second] = await Promise.all([check(), check()]);
    expect([first, second]).toEqual(["renewed", "renewed"]);
    expect(b.subscribe).toHaveBeenCalledTimes(1);
    expect(await check()).toBe("current");
    expect(b.fetchMock.mock.calls.filter(([url]) => url === "/api/push/vapid-public-key")).toHaveLength(2);
  });

  it("holds the browser-wide lock while it checks", async () => {
    const requests: string[] = [];
    Object.defineProperty(navigator, "locks", {
      configurable: true,
      value: {
        request: vi.fn(async (name: string, fn: () => Promise<unknown>) => {
          requests.push(name);
          return fn();
        }),
      },
    });
    stubBrowser({ renew: { renewed: true } });
    expect(await (await loadCheck())()).toBe("renewed");
    expect(requests).toEqual(["fleet-push-subscription"]);
  });
});
