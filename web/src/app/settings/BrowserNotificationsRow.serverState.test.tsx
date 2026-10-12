import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BrowserNotificationsRow } from "./BrowserNotificationsRow";

// The switch reads "on" only when the server holds this browser's
// subscription for the signed-in user. A browser can hold a subscription that
// is someone else's (another account turned notifications on in this browser)
// or one the server has dropped, and a browser-only check showed those as on.

const ENDPOINT = "https://relay.example/ep";

function stubBrowser(opts: {
  sub: { endpoint: string; unsubscribe: () => Promise<boolean> } | null;
  status?: { subscribed: boolean } | number;
}) {
  const requests: string[] = [];
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
      getRegistration: vi.fn(async () => ({
        pushManager: { getSubscription: async () => opts.sub },
      })),
    },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      requests.push(`${init?.method ?? "GET"} ${url}${init?.body ? ` ${String(init.body)}` : ""}`);
      if (url === "/api/push/vapid-public-key") return Response.json({ key: "BKEY" });
      if (url === "/api/push/status") {
        return typeof opts.status === "number"
          ? new Response("boom", { status: opts.status })
          : Response.json(opts.status);
      }
      return new Response(null, { status: 204 });
    }),
  );
  return { requests };
}

async function settledSwitch() {
  const sw = screen.getByTestId("browser-notifications-switch");
  await waitFor(() => expect(sw).not.toBeDisabled());
  return sw;
}

describe("BrowserNotificationsRow reflects the server's record", () => {
  afterEach(() => {
    Reflect.deleteProperty(navigator, "serviceWorker");
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("reads on when the server holds this browser's subscription for the signed-in user", async () => {
    const unsubscribe = vi.fn(async () => true);
    const { requests } = stubBrowser({
      sub: { endpoint: ENDPOINT, unsubscribe },
      status: { subscribed: true },
    });
    render(<BrowserNotificationsRow />);
    const sw = await settledSwitch();
    expect(sw).toHaveAttribute("aria-checked", "true");
    expect(requests).toContain(`POST /api/push/status {"endpoint":"${ENDPOINT}"}`);
    expect(unsubscribe).not.toHaveBeenCalled();
  });

  it("reads off and lets go of a subscription the server does not hold for this user", async () => {
    // Another account's subscription on a shared browser, or one the server
    // dropped: nothing reaches this user through it, and keeping it would keep
    // another account's alerts arriving here.
    const unsubscribe = vi.fn(async () => true);
    stubBrowser({ sub: { endpoint: ENDPOINT, unsubscribe }, status: { subscribed: false } });
    render(<BrowserNotificationsRow />);
    const sw = await settledSwitch();
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });

  it("does not read a failed check as someone else's subscription", async () => {
    const unsubscribe = vi.fn(async () => true);
    stubBrowser({ sub: { endpoint: ENDPOINT, unsubscribe }, status: 500 });
    render(<BrowserNotificationsRow />);
    const sw = await settledSwitch();
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(screen.getByText(/Failed to check notifications: 500/)).toBeInTheDocument();
    expect(unsubscribe).not.toHaveBeenCalled();
  });

  it("asks nothing of the server when the browser holds no subscription", async () => {
    const { requests } = stubBrowser({ sub: null });
    render(<BrowserNotificationsRow />);
    const sw = await settledSwitch();
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(requests.some((r) => r.includes("/api/push/status"))).toBe(false);
  });
});
