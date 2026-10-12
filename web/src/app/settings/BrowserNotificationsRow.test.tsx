import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BrowserNotificationsRow } from "./BrowserNotificationsRow";

// The browser-notification switch (Settings → General) against a fake
// browser: the states it explains instead of offering the switch, and the
// turn-on / turn-off flows. Every case starts from a browser with no push
// subscription, so the flows run end to end through the switch itself.

const KEY_BYTES = (() => {
  const k = new Uint8Array(65).fill(7);
  k[0] = 4;
  return k;
})();
const KEY = Buffer.from(KEY_BYTES)
  .toString("base64")
  .replace(/\+/g, "-")
  .replace(/\//g, "_")
  .replace(/=+$/, "");
const ENDPOINT = "https://relay.example/ep";

type Opts = {
  pushSupported?: boolean;
  permission?: NotificationPermission;
  grant?: NotificationPermission;
  vapidStatus?: number;
  subscribeStatus?: number;
};

function stubBrowser(opts: Opts = {}) {
  const events: string[] = [];
  const browser = { current: null as null | { endpoint: string; unsubscribe: () => Promise<boolean> } };
  const reg = {
    pushManager: {
      getSubscription: async () => browser.current,
      subscribe: vi.fn(async (o: { userVisibleOnly: boolean; applicationServerKey: ArrayBuffer }) => {
        const sameKey = Buffer.from(new Uint8Array(o.applicationServerKey)).equals(Buffer.from(KEY_BYTES));
        events.push(`subscribe userVisibleOnly=${o.userVisibleOnly} serverKey=${sameKey}`);
        const sub = {
          endpoint: ENDPOINT,
          unsubscribe: vi.fn(async () => {
            events.push("browser unsubscribe");
            browser.current = null;
            return true;
          }),
          toJSON: () => ({ endpoint: ENDPOINT, keys: { auth: "auth", p256dh: "p256dh" } }),
        };
        browser.current = sub;
        return sub;
      }),
    },
  };
  if (opts.pushSupported !== false) vi.stubGlobal("PushManager", function PushManager() {});
  const requestPermission = vi.fn(async () => {
    events.push("permission prompt");
    return opts.grant ?? "granted";
  });
  vi.stubGlobal(
    "Notification",
    Object.assign(function Notification() {}, {
      permission: opts.permission ?? "default",
      requestPermission,
    }),
  );
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: {
      getRegistration: vi.fn(async () => (browser.current ? reg : undefined)),
      register: vi.fn(async (url: string) => {
        events.push(`register ${url}`);
        return reg;
      }),
      ready: Promise.resolve(reg),
    },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/push/vapid-public-key") {
        return opts.vapidStatus
          ? new Response(JSON.stringify({ error: "push_disabled" }), { status: opts.vapidStatus })
          : Response.json({ key: KEY });
      }
      events.push(`${init?.method ?? "GET"} ${url} ${String(init?.body ?? "")}`);
      if (url === "/api/push/subscribe" && opts.subscribeStatus) {
        return new Response("could not store the subscription", { status: opts.subscribeStatus });
      }
      return new Response(null, { status: 204 });
    }),
  );
  return { events, requestPermission, reg };
}

const switchEl = () => screen.getByTestId("browser-notifications-switch");

describe("BrowserNotificationsRow", () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("explains, with the switch disabled, a browser without Web Push", async () => {
    stubBrowser({ pushSupported: false });
    render(<BrowserNotificationsRow />);
    expect(await screen.findByText(/doesn't support Web Push notifications/)).toBeInTheDocument();
    expect(switchEl()).toBeDisabled();
  });

  it("explains, with the switch disabled, a server without VAPID keys", async () => {
    stubBrowser({ vapidStatus: 501 });
    render(<BrowserNotificationsRow />);
    expect(await screen.findByText(/not configured on this server/)).toBeInTheDocument();
    expect(switchEl()).toBeDisabled();
  });

  it("explains, with the switch disabled, notifications blocked for the site", async () => {
    stubBrowser({ permission: "denied" });
    render(<BrowserNotificationsRow />);
    expect(await screen.findByText(/Notifications are blocked for this site/)).toBeInTheDocument();
    expect(switchEl()).toBeDisabled();
  });

  it("turns on: permission, the service worker, a subscription on the server's key, stored", async () => {
    const { events } = stubBrowser();
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    expect(switchEl()).toHaveAttribute("aria-checked", "false");

    fireEvent.click(switchEl());
    await waitFor(() => expect(switchEl()).toHaveAttribute("aria-checked", "true"));
    expect(events).toEqual([
      "permission prompt",
      "register /sw.js",
      "subscribe userVisibleOnly=true serverKey=true",
      `POST /api/push/subscribe ${JSON.stringify({ endpoint: ENDPOINT, keys: { auth: "auth", p256dh: "p256dh" } })}`,
    ]);
  });

  it("stays off, subscribing nothing, when the permission prompt is dismissed", async () => {
    const { events } = stubBrowser({ grant: "default" });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    await waitFor(() => expect(events).toContain("permission prompt"));
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
    expect(events).toEqual(["permission prompt"]);
  });

  it("shows the blocked note when the prompt is answered with Block", async () => {
    stubBrowser({ grant: "denied" });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    expect(await screen.findByText(/Notifications are blocked for this site/)).toBeInTheDocument();
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
  });

  it("stays off and says why when the server cannot store the subscription", async () => {
    stubBrowser({ subscribeStatus: 500 });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    expect(await screen.findByText("could not store the subscription")).toBeInTheDocument();
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
  });

  it("turns off: deletes the stored row, then unsubscribes the browser", async () => {
    const { events } = stubBrowser();
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    await waitFor(() => expect(switchEl()).toHaveAttribute("aria-checked", "true"));
    events.length = 0;

    fireEvent.click(switchEl());
    await waitFor(() => expect(switchEl()).toHaveAttribute("aria-checked", "false"));
    expect(events).toEqual([
      `DELETE /api/push/unsubscribe ${JSON.stringify({ endpoint: ENDPOINT })}`,
      "browser unsubscribe",
    ]);
  });
});
