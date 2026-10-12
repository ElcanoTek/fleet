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

describe("signOut", () => {
  afterEach(() => {
    document.body.innerHTML = "";
    Reflect.deleteProperty(navigator, "serviceWorker");
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
