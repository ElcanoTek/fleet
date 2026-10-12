// Fleet service worker (#292): renders Web Push notifications while the tab
// is backgrounded or closed. Payloads are low-detail by design — the server
// sends only { title, body?, url? } (see internal/webpush); anything else is
// ignored. Registered on demand from the settings notifications card.

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
  const target = new URL(event.notification.data?.url || "/", self.location.origin);
  event.waitUntil(openNotificationTarget(target));
});

// openNotificationTarget brings the user to a notification's link: a task's
// record, a chat conversation (/chat?c=<id>), or just Fleet for a link to the
// app root. The browser grants one window action (focus or open) per click, so
// the window is chosen BEFORE that action is spent: an open Fleet tab this
// worker controls is focused and navigated there; with none, the link opens
// in a new window.
async function openNotificationTarget(target) {
  const origin = self.location.origin;
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const showing = windows.find((c) => c.url === target.href);
  if (showing) return showing.focus();
  // A link to another origin (FLEET_PUBLIC_URL naming a different host than
  // the one this browser uses) is opened, never navigated into from here.
  if (target.origin !== origin) return self.clients.openWindow(target.href);
  // The app root names no destination: bring Fleet forward wherever it is.
  if (target.pathname === "/" && !target.search) {
    return windows.length ? windows[0].focus() : self.clients.openWindow(target.href);
  }
  // matchAll without includeUncontrolled lists only the windows this worker
  // controls, most recently focused first.
  const [tab] = await self.clients.matchAll({ type: "window" });
  if (!tab) return self.clients.openWindow(target.href);
  const focused = await tab.focus();
  return focused.navigate(target.href);
}
