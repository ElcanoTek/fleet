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
type RenewAnswer = { renewed: boolean } | "network-error" | number | { status: number; error: string };

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
  renew?: RenewAnswer | ((oldEndpoint: string) => RenewAnswer);
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
      const oldEndpoint = (JSON.parse(String(init?.body)) as { old_endpoint: string }).old_endpoint;
      const answer = typeof opts.renew === "function" ? opts.renew(oldEndpoint) : opts.renew;
      if (answer === "network-error") throw new TypeError("network down");
      if (typeof answer === "number") return new Response("boom", { status: answer });
      if (answer && "status" in answer) return Response.json({ error: answer.error }, { status: answer.status });
      return Response.json(answer ?? { renewed: true });
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
    // No answer: offline, a 5xx, or a session that expired (401, which a
    // sign-in fixes).
    for (const failure of ["network-error", 503, 401] as const) {
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

  it("takes a 400, or a 403 the server marks as its own refusal, as a final no: a Viewer cannot renew, so it is not retried", async () => {
    const refusals: RenewAnswer[] = [
      { status: 403, error: "read_only" },
      { status: 403, error: "not_a_member" },
      400,
    ];
    for (const refusal of refusals) {
      window.localStorage.clear();
      const b = stubBrowser({ renew: refusal });
      expect(await (await loadCheck())()).toBe("dropped");
      expect(b.browser.current).toBeNull();
      expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBeNull();
      vi.unstubAllGlobals();
    }
  });

  it("keeps the new subscription on a 403 that is not the server's refusal, such as the web app's origin check", async () => {
    // A deployment whose public origin is misconfigured rejects every POST
    // with {"error":"forbidden"}; that says nothing about whose subscription
    // this is, so letting it go would turn notifications off on every renewal.
    const failures: RenewAnswer[] = [{ status: 403, error: "forbidden" }, 403];
    for (const failure of failures) {
      window.localStorage.clear();
      const b = stubBrowser({ renew: failure });
      expect(await (await loadCheck())()).toBe("pending");
      expect(b.browser.current?.endpoint).toBe("https://relay.example/new");
      expect(JSON.parse(window.localStorage.getItem("fleet.push.pendingRenewal")!)).toEqual([
        "https://relay.example/old",
      ]);
      vi.unstubAllGlobals();
    }
  });

  it("remembers every unconfirmed endpoint, so a lost answer and a second rotation do not strand the row", async () => {
    // Renewal original → old reached the server but its answer was lost, so
    // "original" is pending and the row now sits at "old". The keys rotate
    // again and this renewal gets no answer either.
    window.localStorage.setItem("fleet.push.pendingRenewal", JSON.stringify(["https://relay.example/original"]));
    const b = stubBrowser({ renew: "network-error" });
    expect(await (await loadCheck())()).toBe("pending");
    expect(JSON.parse(window.localStorage.getItem("fleet.push.pendingRenewal")!)).toEqual([
      "https://relay.example/original",
      "https://relay.example/old",
    ]);
    vi.unstubAllGlobals();

    // Next load, back online: the subscription is on the current key; the
    // server says no for "original" and moves the row from "old".
    const again = stubBrowser({
      subKey: NEW_KEY,
      renew: (oldEndpoint) => ({ renewed: oldEndpoint === "https://relay.example/old" }),
    });
    again.browser.current = b.browser.current;
    expect(await (await loadCheck())()).toBe("renewed");
    expect(again.events.filter((e) => e.startsWith("POST /api/push/renew"))).toEqual([
      renewBody("https://relay.example/original", "https://relay.example/new"),
      renewBody("https://relay.example/old", "https://relay.example/new"),
    ]);
    expect(again.browser.current?.endpoint).toBe("https://relay.example/new");
    expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBeNull();
  });

  it("forgets a pending endpoint the server said no to, and keeps the ones it did not answer", async () => {
    window.localStorage.setItem(
      "fleet.push.pendingRenewal",
      JSON.stringify(["https://relay.example/a", "https://relay.example/b"]),
    );
    const b = stubBrowser({
      subKey: NEW_KEY,
      renew: (oldEndpoint) => (oldEndpoint === "https://relay.example/a" ? { renewed: false } : 503),
    });
    expect(await (await loadCheck())()).toBe("pending");
    expect(b.browser.current?.endpoint).toBe("https://relay.example/old");
    expect(JSON.parse(window.localStorage.getItem("fleet.push.pendingRenewal")!)).toEqual([
      "https://relay.example/b",
    ]);
  });

  it("keeps at most the eight most recent pending endpoints", async () => {
    const many = Array.from({ length: 10 }, (_, i) => `https://relay.example/p${i}`);
    window.localStorage.setItem("fleet.push.pendingRenewal", JSON.stringify(many));
    stubBrowser({ renew: "network-error" });
    expect(await (await loadCheck())()).toBe("pending");
    const kept = JSON.parse(window.localStorage.getItem("fleet.push.pendingRenewal")!) as string[];
    expect(kept).toHaveLength(8);
    expect(kept.at(-1)).toBe("https://relay.example/old");
    expect(kept[0]).toBe("https://relay.example/p3");
  });

  it("after a second rotation, tries the endpoint a pending renewal replaced before this one", async () => {
    // A renewal the server never confirmed left the user's row at the
    // original endpoint, or (the answer was lost) moved it to the current one.
    for (const rowAt of ["https://relay.example/original", "https://relay.example/old"]) {
      window.localStorage.setItem("fleet.push.pendingRenewal", "https://relay.example/original");
      const b = stubBrowser({ renew: (oldEndpoint) => ({ renewed: oldEndpoint === rowAt }) });
      expect(await (await loadCheck())()).toBe("renewed");
      const asked = b.events.filter((e) => e.startsWith("POST /api/push/renew"));
      expect(asked[0]).toBe(renewBody("https://relay.example/original", "https://relay.example/new"));
      expect(asked).toHaveLength(rowAt === "https://relay.example/original" ? 1 : 2);
      expect(b.browser.current?.endpoint).toBe("https://relay.example/new");
      expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBeNull();
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

describe("withPushLock", () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, "locks");
    window.localStorage.clear();
  });

  // A lock another tab holds and never lets go: a request waits until its
  // signal gives up, as the Web Locks API does.
  function stubHeldLock() {
    Object.defineProperty(navigator, "locks", {
      configurable: true,
      value: {
        request: vi.fn(
          (_name: string, opts: { signal?: AbortSignal }) =>
            new Promise((_resolve, reject) => {
              opts.signal?.addEventListener("abort", () => reject(opts.signal?.reason));
            }),
        ),
      },
    });
  }

  it("with a deadline, goes ahead without the lock once the wait is over", async () => {
    stubHeldLock();
    const { withPushLock } = await import("./pushKey");
    const fn = vi.fn(async () => "done");
    expect(await withPushLock(fn, { waitMs: 20 })).toBe("done");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("with a deadline, runs under the lock when it is free, and does not run twice on an error", async () => {
    const held: boolean[] = [];
    let holding = false;
    Object.defineProperty(navigator, "locks", {
      configurable: true,
      value: {
        request: vi.fn(async (_name: string, _opts: unknown, fn: () => Promise<unknown>) => {
          holding = true;
          try {
            return await fn();
          } finally {
            holding = false;
          }
        }),
      },
    });
    const { withPushLock } = await import("./pushKey");
    const fn = vi.fn(async () => {
      held.push(holding);
      throw new Error("boom");
    });
    await expect(withPushLock(fn, { waitMs: 1000 })).rejects.toThrow("boom");
    expect(held).toEqual([true]);
  });

  it("takePendingRenewal hands over the remembered endpoints once", async () => {
    const { takePendingRenewal } = await import("./pushKey");
    window.localStorage.setItem(
      "fleet.push.pendingRenewal",
      JSON.stringify(["https://relay.example/original", "https://relay.example/old"]),
    );
    expect(takePendingRenewal()).toEqual(["https://relay.example/original", "https://relay.example/old"]);
    expect(takePendingRenewal()).toEqual([]);
    // The single endpoint an earlier version stored.
    window.localStorage.setItem("fleet.push.pendingRenewal", "https://relay.example/original");
    expect(takePendingRenewal()).toEqual(["https://relay.example/original"]);
    expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBeNull();
  });
});
