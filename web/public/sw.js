// Fleet service worker (#292): renders Web Push notifications while the tab
// is backgrounded or closed. Payloads are low-detail by design — the server
// sends only { title, body?, url? } (see internal/webpush); anything else is
// ignored. Registered on demand from the settings notifications card.

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
  const target = new URL(event.notification.data?.url || self.location.origin, self.location.origin).href;
  // A chat notification links one conversation (/chat?c=<id>, which the chat
  // page opens on load). Only a same-origin link is ever navigated to.
  const sameOrigin = target.startsWith(self.location.origin);
  const deepLink = sameOrigin && new URL(target).searchParams.has("c");
  event.waitUntil(
    clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
      // Focus an existing Fleet window if one is open; otherwise open the
      // notification's deep link (same-origin targets only reach this worker).
      const fleet = list.find((c) => c.url.startsWith(self.location.origin));
      if (!fleet) return clients.openWindow(target);
      if (!deepLink) return fleet.focus();
      // Bring that window to the conversation. navigate() only works on a
      // window this worker controls; otherwise open the link in a new one.
      return fleet
        .focus()
        .then((c) => c.navigate(target))
        .catch(() => clients.openWindow(target));
    }),
  );
});
