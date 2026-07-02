"use client";

import { useEffect, useState } from "react";

// Browser notifications opt-in (#292). Enabling walks the standard Web Push
// flow: Notification.requestPermission() → register /sw.js → subscribe with
// the server's VAPID public key (fetched, never build-time embedded) → store
// the subscription server-side. Disabling unsubscribes locally AND deletes
// the stored row. Notifications are low-detail by design (task done/failed,
// approval needed, waiting for an answer — never message content).

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

export default function NotificationsCard() {
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

  return (
    <div className="mt-6 rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)] p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h2 className="text-[0.9rem] font-semibold">Browser notifications</h2>
          <p className="mt-1 text-[0.75rem] text-[var(--color-text-muted)]">
            Get an alert when a task finishes, needs an approval, or is waiting for your answer —
            even when this tab is in the background. Alerts carry only the task name and state,
            never message content.
          </p>
        </div>
        {state === "enabled" ? (
          <button
            type="button"
            onClick={disable}
            disabled={busy}
            className="rounded-full border border-[var(--color-border-subtle)] px-4 py-2 text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
          >
            {busy ? "Working…" : "Disable"}
          </button>
        ) : state === "disabled" ? (
          <button
            type="button"
            onClick={enable}
            disabled={busy}
            className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
          >
            {busy ? "Working…" : "Enable notifications"}
          </button>
        ) : null}
      </div>
      {state === "enabled" ? (
        <p className="mt-2 text-[0.75rem] text-[#7fd6a6]">Notifications are on in this browser.</p>
      ) : null}
      {state === "unsupported" ? (
        <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
          This browser doesn&apos;t support Web Push notifications. Chrome, Edge, Firefox, and
          Safari 16.4+ do.
        </p>
      ) : null}
      {state === "denied" ? (
        <p className="mt-2 text-[0.75rem] text-[#e0b080]">
          Notifications are blocked for this site. Allow them in your browser&apos;s site settings,
          then reload this page.
        </p>
      ) : null}
      {state === "unconfigured" ? (
        <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
          Push notifications are not configured on this server. The operator must run{" "}
          <code className="rounded bg-[var(--color-overlay-soft)] px-1">
            fleet generate-vapid-keys
          </code>{" "}
          and set FLEET_VAPID_PUBLIC_KEY, FLEET_VAPID_PRIVATE_KEY, and FLEET_VAPID_CONTACT.
        </p>
      ) : null}
      {error ? <p className="mt-2 text-[0.75rem] text-[#e08080]">{error}</p> : null}
    </div>
  );
}
