// The browser's own Web Push subscription (#292). A subscription belongs to the
// account that turned notifications on in this browser, so signing out lets
// it go: the next person to use the browser must not receive that account's
// alerts.

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
// row on its next send's 404/410.
export async function dropPushSubscription(): Promise<void> {
  const sub = await browserPushSubscription();
  if (!sub) return;
  await Promise.allSettled([
    fetch("/api/push/unsubscribe", {
      method: "DELETE",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ endpoint: sub.endpoint }),
      keepalive: true,
    }),
    sub.unsubscribe(),
  ]);
}
