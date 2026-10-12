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

function fakeSub(endpoint: string, key: Uint8Array, events: string[]) {
  return {
    endpoint,
    options: { applicationServerKey: key.buffer },
    unsubscribe: vi.fn(async () => {
      events.push(`unsubscribe ${endpoint}`);
      return true;
    }),
    toJSON: () => ({ endpoint, keys: { auth: "auth", p256dh: "p256dh" } }),
  };
}

function stubBrowser(opts: {
  permission?: NotificationPermission;
  subKey?: Uint8Array | null;
  serverKey?: Uint8Array;
  vapidStatus?: number;
  renew?: { renewed: boolean } | "network-error";
}) {
  const events: string[] = [];
  const old = opts.subKey === null ? null : fakeSub("https://relay.example/old", opts.subKey ?? OLD_KEY, events);
  const fresh = fakeSub("https://relay.example/new", opts.serverKey ?? NEW_KEY, events);
  const subscribe = vi.fn(async (o: { applicationServerKey: ArrayBuffer }) => {
    events.push(`subscribe ${b64url(new Uint8Array(o.applicationServerKey))}`);
    return fresh;
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
        pushManager: { getSubscription: async () => old, subscribe },
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
      return Response.json(opts.renew ?? { renewed: true });
    }
    return new Response(null, { status: 404 });
  });
  vi.stubGlobal("fetch", fetchMock);
  return { events, old, fresh, subscribe, fetchMock };
}

// A fresh module per test: the check is memoized per page load.
async function loadCheck() {
  vi.resetModules();
  return (await import("./pushKey")).ensureCurrentPushKey;
}

describe("ensureCurrentPushKey", () => {
  beforeEach(() => {
    vi.resetModules();
  });
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
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
      `POST /api/push/renew ${JSON.stringify({
        old_endpoint: "https://relay.example/old",
        endpoint: "https://relay.example/new",
        keys: { auth: "auth", p256dh: "p256dh" },
      })}`,
    ]);
    expect(b.fresh.unsubscribe).not.toHaveBeenCalled();
  });

  it("lets the new subscription go when the old one was not this user's", async () => {
    // Another account's subscription on a shared browser: renewing must not
    // sign up whoever is signed in now.
    const b = stubBrowser({ renew: { renewed: false } });
    expect(await (await loadCheck())()).toBe("dropped");
    expect(b.fresh.unsubscribe).toHaveBeenCalledTimes(1);
  });

  it("lets the new subscription go when the renewal cannot reach the server", async () => {
    const b = stubBrowser({ renew: "network-error" });
    expect(await (await loadCheck())()).toBe("dropped");
    expect(b.fresh.unsubscribe).toHaveBeenCalledTimes(1);
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

  it("runs once per page load however many callers ask", async () => {
    const b = stubBrowser({ renew: { renewed: true } });
    const check = await loadCheck();
    const [first, second] = await Promise.all([check(), check()]);
    expect([first, second]).toEqual(["renewed", "renewed"]);
    expect(b.subscribe).toHaveBeenCalledTimes(1);
    expect(b.fetchMock.mock.calls.filter(([url]) => url === "/api/push/vapid-public-key")).toHaveLength(1);
  });
});
