"use client";

// Browser notifications preference row (Settings → General). The Web Push
// opt-in that used to live on the Connections page as NotificationsCard
// (#292), re-expressed as the design's preference row: label + description
// left, a switch right. Enabling walks the standard flow —
// Notification.requestPermission() → register /sw.js → subscribe with the
// server's VAPID public key (fetched, never build-time embedded) → store the
// subscription server-side. Disabling unsubscribes locally AND deletes the
// stored row. The extra real-world states the design doesn't show
// (unsupported browser / operator hasn't configured VAPID / permission
// denied) render as a note under the description with the switch disabled.

import { useEffect, useState } from "react";
import { SetRow, SetSwitch } from "./ui/atoms";

type PushState =
  | "loading" // probing support + current subscription
  | "unsupported" // browser has no service worker / Push API
  | "unconfigured" // backend answered 501 — operator hasn't set VAPID keys
  | "denied" // the user blocked notifications for this site
  | "disabled" // supported + configured, not subscribed
  | "enabled"; // an active subscription is stored

// urlBase64ToUint8Array decodes the base64url VAPID public key into the
// BufferSource shape pushManager.subscribe expects.
function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padding = "=".repeat((4 - (base64.length % 4)) % 4);
  const raw = window.atob((base64 + padding).replace(/-/g, "+").replace(/_/g, "/"));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function pushSupported(): boolean {
  return "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
}

// fetchVapidKey returns the server's VAPID public key, null when the backend
// reports the feature unconfigured (501), and throws on other failures.
async function fetchVapidKey(): Promise<string | null> {
  const res = await fetch("/api/push/vapid-public-key", { cache: "no-store" });
  if (res.status === 501) return null;
  if (!res.ok) throw new Error(`Failed to load push config: ${res.status}`);
  const data = (await res.json()) as { key?: string };
  if (!data.key) return null;
  return data.key;
}

export function BrowserNotificationsRow() {
  const [state, setState] = useState<PushState>("loading");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let stale = false;
    (async () => {
      if (!pushSupported()) return "unsupported" as const;
      const key = await fetchVapidKey();
      if (key === null) return "unconfigured" as const;
      if (Notification.permission === "denied") return "denied" as const;
      const reg = await navigator.serviceWorker.getRegistration("/sw.js");
      const sub = await reg?.pushManager.getSubscription();
      return sub ? ("enabled" as const) : ("disabled" as const);
    })()
      .then((next) => {
        if (!stale) setState(next);
      })
      .catch((e: unknown) => {
        if (stale) return;
        setState("disabled");
        setError(e instanceof Error ? e.message : "Something went wrong.");
      });
    return () => {
      stale = true;
    };
  }, []);

  const enable = async () => {
    setError(null);
    setBusy(true);
    try {
      const permission = await Notification.requestPermission();
      if (permission !== "granted") {
        setState(permission === "denied" ? "denied" : "disabled");
        return;
      }
      const key = await fetchVapidKey();
      if (key === null) {
        setState("unconfigured");
        return;
      }
      const reg = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;
      const sub =
        (await reg.pushManager.getSubscription()) ??
        (await reg.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: urlBase64ToUint8Array(key).buffer as ArrayBuffer,
        }));
      const res = await fetch("/api/push/subscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(sub.toJSON()),
      });
      if (res.status === 501) {
        setState("unconfigured");
        return;
      }
      if (!res.ok && res.status !== 204) {
        throw new Error((await res.text()) || `Subscribe failed: ${res.status}`);
      }
      setState("enabled");
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Something went wrong.");
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    setError(null);
    setBusy(true);
    try {
      const reg = await navigator.serviceWorker.getRegistration("/sw.js");
      const sub = await reg?.pushManager.getSubscription();
      if (sub) {
        // Best-effort server delete first (we still have the endpoint), then
        // drop the browser-side subscription.
        await fetch("/api/push/unsubscribe", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ endpoint: sub.endpoint }),
        });
        await sub.unsubscribe();
      }
      setState("disabled");
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Something went wrong.");
    } finally {
      setBusy(false);
    }
  };

  const note =
    state === "unsupported" ? (
      <span className="text-[var(--color-text-muted)]">
        This browser doesn&apos;t support Web Push notifications. Chrome, Edge, Firefox, and Safari
        16.4+ do.
      </span>
    ) : state === "denied" ? (
      <span className="text-[var(--color-warning-soft)]">
        Notifications are blocked for this site. Allow them in your browser&apos;s site settings,
        then reload this page.
      </span>
    ) : state === "unconfigured" ? (
      <span className="text-[var(--color-text-muted)]">
        Push notifications are not configured on this server — ask your operator to set them up.
      </span>
    ) : error ? (
      <span className="text-[var(--color-danger-soft)]">{error}</span>
    ) : null;

  return (
    <SetRow
      label="Browser notifications"
      desc={
        <>
          Get an alert when a task finishes, needs an approval, or is waiting for your answer —
          even when this tab is in the background. Alerts carry only the task name and state, never
          message content.
          {note ? <span className="mt-[0.3rem] block">{note}</span> : null}
        </>
      }
    >
      <SetSwitch
        on={state === "enabled"}
        onToggle={() => {
          if (busy) return;
          if (state === "enabled") void disable();
          else if (state === "disabled") void enable();
        }}
        disabled={busy || !(state === "enabled" || state === "disabled")}
        label="Browser notifications"
        testId="browser-notifications-switch"
      />
    </SetRow>
  );
}

export default BrowserNotificationsRow;
