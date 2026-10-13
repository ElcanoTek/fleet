// The browser's own Web Push subscription (#292). A subscription belongs to the
// account that turned notifications on in this browser, so signing out lets
// it go: the next person to use the browser must not receive that account's
// alerts.

import { takePendingRenewal, withPushLock } from "./pushKey";

// browserPushSubscription returns this browser's subscription, or null when
// it has none — or has no service worker support at all.
export async function browserPushSubscription(): Promise<PushSubscription | null> {
  if (typeof navigator === "undefined" || !("serviceWorker" in navigator)) return null;
  const reg = await navigator.serviceWorker.getRegistration("/sw.js");
  return (await reg?.pushManager?.getSubscription()) ?? null;
}

// dropPushSubscription deletes the signed-in user's stored row for this
// browser and unsubscribes the browser, both at once. The browser's own
// unsubscribe is what stops delivery, so it never waits behind the server
// round trip: sign-out bounds this clean-up, and a slow delete must not
// leave the browser subscribed. The delete is sent with keepalive so it can
// finish after the page navigates away; if it fails, the server reaps the
// row on its next send's 404/410. A renewal after a key change that the
// server never confirmed still has this account's row at the endpoint it
// replaced, so that row is deleted too.
//
// It runs under the push lock, so it cannot read the subscription while
// another tab is halfway through replacing it (and then miss the renewed
// one), but it waits at most PUSH_LOCK_WAIT_MS for the lock and then goes
// ahead anyway: sign-out has its own deadline.
const PUSH_LOCK_WAIT_MS = 1000;

export function dropPushSubscription(): Promise<void> {
  return withPushLock(async () => {
    const sub = await browserPushSubscription();
    const endpoints = [sub?.endpoint, ...takePendingRenewal()].filter(
      (e, i, all): e is string => !!e && all.indexOf(e) === i,
    );
    await Promise.allSettled([
      ...endpoints.map((endpoint) =>
        fetch("/api/push/unsubscribe", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ endpoint }),
          keepalive: true,
        }),
      ),
      sub?.unsubscribe(),
    ]);
  }, { waitMs: PUSH_LOCK_WAIT_MS });
}
