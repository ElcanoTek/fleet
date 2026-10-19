import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BrowserNotificationsRow } from "./BrowserNotificationsRow";

// The browser-notification switch (Settings → General) against a fake
// browser: the states it explains instead of offering the switch, and the
// turn-on / turn-off flows. Every case starts from a browser with no push
// subscription, so the flows run end to end through the switch itself.

// 0xfb bytes encode to "-" and "_" in base64url, so the key exercises the
// alphabet swap a plain base64 decode would choke on.
const KEY_BYTES = (() => {
  const k = new Uint8Array(65).fill(0xfb);
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
  // Whether /sw.js is already registered. A real registration outlives an
  // unsubscribe, so "registered, no subscription" is the usual state after
  // someone turns notifications off — and the default here.
  registered?: boolean;
  permission?: NotificationPermission;
  grant?: NotificationPermission;
  vapidStatus?: number;
  subscribeStatus?: number;
  // The body of a refused store; the default is a plain-text server error.
  subscribeBody?: string;
  // The signed-in account's role from GET /api/me/team ("member" by default),
  // or an HTTP status for a read that fails. With roleGate, the read answers
  // only once the gate resolves.
  role?: string | number;
  roleGate?: Promise<void>;
};

// stubBrowser fakes the Push API, the service worker container and fetch, and
// records each step the switch takes in `events`, in order. The VAPID key and
// account reads are left out of the log: they happen on every load and are not
// a step of either flow.
function stubBrowser(opts: Opts = {}) {
  const events: string[] = [];
  let registered = opts.registered ?? true;
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
  const notification = Object.assign(function Notification() {}, {
    permission: opts.permission ?? "default",
    requestPermission: vi.fn(async () => {
      events.push("permission prompt");
      notification.permission = opts.grant ?? "granted";
      return notification.permission;
    }),
  });
  vi.stubGlobal("Notification", notification);
  Object.defineProperty(navigator, "serviceWorker", {
    configurable: true,
    value: {
      getRegistration: vi.fn(async () => (registered ? reg : undefined)),
      register: vi.fn(async (url: string) => {
        events.push(`register ${url}`);
        registered = true;
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
      if (url === "/api/me/team") {
        await opts.roleGate;
        const role = opts.role ?? "member";
        return typeof role === "number"
          ? new Response("boom", { status: role })
          : Response.json({ email: "e2e@example.com", role, team_id: "", admin: false });
      }
      events.push(`${init?.method ?? "GET"} ${url} ${String(init?.body ?? "")}`);
      if (url === "/api/push/subscribe" && opts.subscribeStatus) {
        return new Response(opts.subscribeBody ?? "could not store the subscription", {
          status: opts.subscribeStatus,
        });
      }
      return new Response(null, { status: 204 });
    }),
  );
  return { events };
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

  it("explains, with the switch disabled, a Viewer account, without offering the switch first", async () => {
    // The account read answers only after the browser checks have settled:
    // the switch must not come on in between, or a Viewer could press it.
    let answer = () => {};
    const roleGate = new Promise<void>((resolve) => {
      answer = resolve;
    });
    const { events } = stubBrowser({ role: "viewer", roleGate });
    render(<BrowserNotificationsRow />);
    // Long enough for the browser checks, all fakes, to settle.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(switchEl()).toBeDisabled();

    answer();
    expect(await screen.findByText(/Viewer accounts are read-only/)).toBeInTheDocument();
    expect(switchEl()).toBeDisabled();
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
    expect(events).toEqual([]);
  });

  it("tells a Viewer about the account, not about a browser setting that would not help", async () => {
    // Allowing notifications in the browser would still leave a Viewer unable
    // to turn them on, so that advice is not given.
    stubBrowser({ permission: "denied", role: "viewer" });
    render(<BrowserNotificationsRow />);
    expect(await screen.findByText(/Viewer accounts are read-only/)).toBeInTheDocument();
    expect(screen.queryByText(/blocked for this site/)).toBeNull();
    expect(switchEl()).toBeDisabled();
  });

  it("explains a Viewer the account read missed once the server refuses the store", async () => {
    // The read failed (or the role changed after the page loaded), so the
    // switch was offered; the server's refusal ends in the same note, not in
    // its raw JSON.
    stubBrowser({ role: 500, subscribeStatus: 403, subscribeBody: JSON.stringify({ error: "read_only" }) });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    expect(await screen.findByText(/Viewer accounts are read-only/)).toBeInTheDocument();
    expect(switchEl()).toBeDisabled();
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
    expect(screen.queryByText(/read_only/)).toBeNull();
  });

  it("does not take another 403 for a Viewer: it is reported as it came", async () => {
    // The web app's own origin check also answers 403; that is no reason to
    // grey the switch out for good.
    stubBrowser({ subscribeStatus: 403, subscribeBody: JSON.stringify({ error: "forbidden" }) });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    expect(await screen.findByText('{"error":"forbidden"}')).toBeInTheDocument();
    expect(screen.queryByText(/Viewer accounts/)).toBeNull();
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
  });

  it("turns on: permission, the service worker, a subscription on the server's key, stored", async () => {
    // First time in this browser: nothing registered yet.
    const { events } = stubBrowser({ registered: false });
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
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
    expect(events).toEqual(["permission prompt"]);
  });

  it("shows the blocked note when the prompt is answered with Block", async () => {
    const { events } = stubBrowser({ grant: "denied" });
    render(<BrowserNotificationsRow />);
    await waitFor(() => expect(switchEl()).not.toBeDisabled());
    fireEvent.click(switchEl());
    expect(await screen.findByText(/Notifications are blocked for this site/)).toBeInTheDocument();
    expect(switchEl()).toHaveAttribute("aria-checked", "false");
    expect(switchEl()).toBeDisabled();
    expect(events).toEqual(["permission prompt"]);
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
