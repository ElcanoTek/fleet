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
//   opens in a new window, or, in a browser that opens none from a click
//   (Safari), in the most recently used tab (see openOrReuse).
// - Only top-level windows count: a same-origin frame inside a page (an
//   email preview, say) is a window client too, but it is not a tab.
// - The browser grants one window action (focus or open) per click, so the
//   tab is chosen before that action is spent, and navigate() — which needs
//   no such grant, only a tab this worker controls — runs whether or not the
//   focus succeeds.
async function openNotificationTarget(link) {
  const origin = self.location.origin;
  const given = new URL(link || "/", origin);
  // Each part is assigned onto a URL of this origin rather than re-parsed as
  // a string: a path beginning "//" (https://a.example//b.example/x) would
  // otherwise read as a link to another host. Assigning never changes the host.
  const target = new URL(origin);
  target.pathname = given.pathname;
  target.search = given.search;
  target.hash = given.hash;
  const section = target.pathname.split("/")[1] || "";
  if (!section) {
    // The app root names no destination: bring the app forward wherever it is.
    const [any] = topLevel(await self.clients.matchAll({ type: "window", includeUncontrolled: true }));
    return any ? any.focus() : self.clients.openWindow(target.href);
  }
  // matchAll without includeUncontrolled lists only the windows this worker
  // controls, most recently focused first.
  const tabs = topLevel(await self.clients.matchAll({ type: "window" }));
  const tab = tabs.find((c) => new URL(c.url).pathname.split("/")[1] === section);
  if (!tab) return openOrReuse(target.href);
  await tab.focus().catch(() => undefined);
  return tab.navigate(target.href);
}

// topLevel keeps the window clients that are tabs, dropping same-origin frames.
const topLevel = (list) => list.filter((c) => c.frameType === "top-level");

// isWebKit says whether this worker runs in WebKit: Safari, and every browser
// on iOS and iPadOS. Chromium's user agent carries "AppleWebKit" too, so it is
// told apart by its "Chrome/" (or "Chromium/") token; Firefox carries neither.
function isWebKit() {
  const ua = self.navigator.userAgent;
  return /AppleWebKit\//.test(ua) && !/(Chrome|Chromium)\//.test(ua);
}

// LATE_WINDOW_MS is how long a click in WebKit waits, after openWindow came
// back without a window, for one that opens late. It must stay well under
// WebKit's 2 s user-gesture lifetime for a notification click
// (ServiceWorkerGlobalScope::userGestureLifetime): past it, focus() is refused
// and the tab the click reuses would change in the background.
const LATE_WINDOW_MS = 1000;

// openOrReuse opens href in a new window. In WebKit, where a click's openWindow
// opens nothing, it sends the most recently used fleet tab there instead.
//
// Safari (26.5 on macOS, tested) answers openWindow from a notification click
// with null and opens no window, so a click there did nothing unless a tab in
// the link's part of the app was open; focus() and navigate() still work. The
// fallback is WebKit's alone: Chromium also answers null when the window it
// opened shows no page of this origin (an error page while fleet is
// unreachable), and reusing a tab then would reload it for nothing.
//
// The windows open before the click are noted first. WebKit is reported to
// answer openWindow before a window it did open exists (WebKit bug 279456,
// seen on iOS; not seen on macOS), so after LATE_WINDOW_MS a window that was
// not there before is taken to be that one, wherever it was redirected, and is
// brought forward. Otherwise the most recently used tab this worker controls
// is brought forward and navigated, which reloads it and loses anything
// unsaved in it, a trade made on purpose over a click that does nothing. With
// only a tab the worker does not control (after a hard reload), that one is
// brought forward, since it cannot be navigated.
async function openOrReuse(href) {
  if (!isWebKit()) return self.clients.openWindow(href);
  const listWindows = async () => topLevel(await self.clients.matchAll({ type: "window", includeUncontrolled: true }));
  const before = new Set((await listWindows()).map((c) => c.id));
  const opened = await self.clients.openWindow(href).catch(() => null);
  if (opened) return opened;
  await new Promise((resolve) => setTimeout(resolve, LATE_WINDOW_MS));
  const windows = await listWindows();
  const late = windows.find((c) => !before.has(c.id));
  if (late) return late.focus().catch(() => null);
  // Only a tab this worker controls can be navigated; most recent first.
  const [tab] = topLevel(await self.clients.matchAll({ type: "window" }));
  if (!tab) return windows[0] ? windows[0].focus().catch(() => null) : null;
  await tab.focus().catch(() => undefined);
  return tab.navigate(href);
}
