// Fleet service worker (#292): renders Web Push notifications while the tab
// is backgrounded or closed. Payloads are low-detail by design — the server
// sends only { title, body?, url? } (see internal/webpush); anything else is
// ignored. Registered on demand from the Browser notifications switch
// (Settings → General).

// Take effect at once, and take control of the Fleet tabs that were already
// open when this worker activated (the settings tab that turned notifications
// on is one): a notification click can only navigate a tab this worker
// controls. The worker has no fetch handler, so controlling a page changes
// nothing else about it.
self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  let payload = { title: "Fleet", body: "" };
  try {
    payload = event.data?.json() ?? payload;
  } catch {
    // A malformed payload still surfaces a generic notification.
  }
  const { title, body, url } = payload;
  event.waitUntil(
    self.registration.showNotification(title || "Fleet", {
      body: body || "",
      icon: "/app-icons/icon-192.png",
      // No dedicated monochrome badge asset exists yet; the platform falls
      // back to a generic glyph when the field is omitted.
      data: { url: url || self.location.origin },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(openNotificationTarget(event.notification.data?.url));
});

// openNotificationTarget brings the user to a notification's link: a task's
// record (/orchestrator/tasks/<id>), a chat conversation (/chat?c=<id>), or
// just the app for a link to its root.
//
// - The link is moved onto this worker's origin. FLEET_PUBLIC_URL can name a
//   different host than the one this browser uses, but the subscription was
//   made here, and the session cookie lives here.
// - Only a tab already in the link's section (the Operations Center for a
//   task, Chat for a conversation) is navigated. Navigating reloads the page,
//   so anything unsaved in that tab is lost; a tab elsewhere in the app is
//   never reloaded for a link it was not showing. With no such tab the link
//   opens in a new window.
// - Only top-level windows count: a same-origin frame inside a page (an
//   email preview, say) is a window client too, but it is not a tab.
// - The browser grants one window action (focus or open) per click, so the
//   tab is chosen before that action is spent, and navigate() — which needs
//   no such grant, only a tab this worker controls — runs whether or not the
//   focus succeeds.
async function openNotificationTarget(link) {
  const origin = self.location.origin;
  const given = new URL(link || "/", origin);
  const target = new URL(given.pathname + given.search + given.hash, origin);
  const section = target.pathname.split("/")[1] || "";
  const topLevel = (list) => list.filter((c) => c.frameType === "top-level");
  if (!section) {
    // The app root names no destination: bring the app forward wherever it is.
    const [any] = topLevel(await self.clients.matchAll({ type: "window", includeUncontrolled: true }));
    return any ? any.focus() : self.clients.openWindow(target.href);
  }
  // matchAll without includeUncontrolled lists only the windows this worker
  // controls, most recently focused first.
  const tabs = topLevel(await self.clients.matchAll({ type: "window" }));
  const tab = tabs.find((c) => new URL(c.url).pathname.split("/")[1] === section);
  if (!tab) return self.clients.openWindow(target.href);
  await tab.focus().catch(() => undefined);
  return tab.navigate(target.href);
}
