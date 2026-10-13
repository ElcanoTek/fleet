import { afterEach, describe, expect, it, vi } from "vitest";
import { signOut, signOutAfter } from "./signOut";

// stubPushSubscription gives jsdom (which has no service workers) a browser
// holding one push subscription, whose lookup can be made to hang.
function stubPushSubscription(
  sub: { endpoint: string; unsubscribe: () => Promise<boolean> },
  getSubscription: () => Promise<unknown> = async () => sub,
) {
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: { getRegistration: async () => ({ pushManager: { getSubscription } }) },
  });
}

// stubLocks installs a Web Locks API that grants one holder at a time, in
// order, and gives up on a waiting request when its signal aborts.
function stubLocks() {
  let queue: Promise<unknown> = Promise.resolve();
  Object.defineProperty(navigator, "locks", {
    configurable: true,
    value: {
      request: (_name: string, a: unknown, b?: () => Promise<unknown>) => {
        const fn = (b ?? a) as () => Promise<unknown>;
        const signal = b ? (a as { signal?: AbortSignal }).signal : undefined;
        let granted = false;
        const run = queue.then(() => {
          if (signal?.aborted) throw signal.reason;
          granted = true;
          return fn();
        });
        queue = run.catch(() => undefined);
        if (!signal) return run;
        return new Promise((resolve, reject) => {
          run.then(resolve, reject);
          signal.addEventListener("abort", () => {
            if (!granted) reject(signal.reason);
          });
        });
      },
    },
  });
}

describe("signOut", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    Reflect.deleteProperty(navigator, "serviceWorker");
    Reflect.deleteProperty(navigator, "locks");
    window.localStorage.clear();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("submits a top-level POST form to /api/auth/logout", async () => {
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    await signOut();
    const form = document.querySelector("form");
    expect(form).not.toBeNull();
    expect(form!.method).toBe("post");
    expect(new URL(form!.action, "https://fleet.example").pathname).toBe(
      "/api/auth/logout",
    );
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("signOutAfter signs out once the cleanup settles, whether it resolved or rejected", async () => {
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    await signOutAfter(Promise.resolve("ok"));
    await signOutAfter(Promise.reject(new Error("orchestrator down")));
    expect(submit).toHaveBeenCalledTimes(2);
  });

  it("lets go of this browser's push subscription before the logout form posts", async () => {
    // The subscription belongs to the account signing out; the next person to
    // use this browser must not receive its notifications.
    const events: string[] = [];
    stubPushSubscription({
      endpoint: "https://relay.example/ep",
      unsubscribe: async () => {
        events.push("browser unsubscribe");
        return true;
      },
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit) => {
        events.push(`${init.method} ${url} ${String(init.body)}`);
        // The delete may outlive the page the logout form navigates away from.
        expect(init.keepalive).toBe(true);
        return new Response(null, { status: 204 });
      }),
    );
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {
      events.push("logout form");
    });
    await signOut();
    expect(events).toEqual([
      `DELETE /api/push/unsubscribe {"endpoint":"https://relay.example/ep"}`,
      "browser unsubscribe",
      "logout form",
    ]);
  });

  it("still unsubscribes the browser when the server delete fails", async () => {
    const unsubscribe = vi.fn(async () => true);
    stubPushSubscription({ endpoint: "https://relay.example/ep", unsubscribe });
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );
    const submit = vi
      .spyOn(HTMLFormElement.prototype, "submit")
      .mockImplementation(() => {});
    await signOut();
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("unsubscribes the browser without waiting for the server delete", async () => {
    // The browser's unsubscribe is what stops delivery. A delete stuck behind
    // a slow server must not leave the browser subscribed when the bounded
    // clean-up gives up and the logout form posts.
    vi.useFakeTimers();
    try {
      const unsubscribe = vi.fn(async () => true);
      stubPushSubscription({ endpoint: "https://relay.example/ep", unsubscribe });
      vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
      const submit = vi
        .spyOn(HTMLFormElement.prototype, "submit")
        .mockImplementation(() => {});
      const done = signOut();
      await vi.advanceTimersByTimeAsync(0);
      expect(unsubscribe).toHaveBeenCalledTimes(1);
      expect(submit).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1500);
      await done;
      expect(submit).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not let a hung push clean-up hold the sign-out", async () => {
    vi.useFakeTimers();
    try {
      stubPushSubscription(
        { endpoint: "https://relay.example/ep", unsubscribe: async () => true },
        () => new Promise(() => {}),
      );
      const submit = vi
        .spyOn(HTMLFormElement.prototype, "submit")
        .mockImplementation(() => {});
      const done = signOut();
      await vi.advanceTimersByTimeAsync(1499);
      expect(submit).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await done;
      expect(submit).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("also deletes the row a key renewal the server never confirmed left behind", async () => {
    // That row is still the signing-out account's, at the endpoint the
    // renewal replaced.
    window.localStorage.setItem("fleet.push.pendingRenewal", "https://relay.example/old");
    const unsubscribe = vi.fn(async () => true);
    stubPushSubscription({ endpoint: "https://relay.example/new", unsubscribe });
    const deleted: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        deleted.push((JSON.parse(String(init.body)) as { endpoint: string }).endpoint);
        return new Response(null, { status: 204 });
      }),
    );
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    await signOut();
    expect(deleted.sort()).toEqual(["https://relay.example/new", "https://relay.example/old"]);
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(window.localStorage.getItem("fleet.push.pendingRenewal")).toBeNull();
  });

  it("waits for a key renewal running in another tab, then lets the renewed subscription go", async () => {
    // Mid-renewal the browser briefly holds no subscription; reading it then
    // would miss the one the renewal is about to store for this account.
    stubLocks();
    const renewed = { endpoint: "https://relay.example/renewed", unsubscribe: vi.fn(async () => true) };
    let current: typeof renewed | null = null;
    stubPushSubscription(renewed, async () => current);
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 204 })));
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    let finishRenewal!: () => void;
    const otherTab = navigator.locks.request(
      "fleet-push-subscription",
      () =>
        new Promise<void>((resolve) => {
          finishRenewal = () => {
            current = renewed;
            resolve();
          };
        }),
    );
    const done = signOut();
    await new Promise((r) => setTimeout(r, 50));
    expect(renewed.unsubscribe).not.toHaveBeenCalled();
    finishRenewal();
    await otherTab;
    await done;
    expect(renewed.unsubscribe).toHaveBeenCalledTimes(1);
  });

  it("does not wait on a push lock that another tab holds for too long", async () => {
    stubLocks();
    void navigator.locks.request("fleet-push-subscription", () => new Promise(() => {}));
    const unsubscribe = vi.fn(async () => true);
    stubPushSubscription({ endpoint: "https://relay.example/ep", unsubscribe });
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 204 })));
    const submit = vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(() => {});
    await signOut();
    // The clean-up went ahead without the lock, inside sign-out's own bound.
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("signOutAfter does not let a hung cleanup hold the sign-out", async () => {
    vi.useFakeTimers();
    try {
      const submit = vi
        .spyOn(HTMLFormElement.prototype, "submit")
        .mockImplementation(() => {});
      const never = new Promise<void>(() => {});
      const done = signOutAfter(never, 1500);
      expect(submit).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1500);
      await done;
      expect(submit).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
