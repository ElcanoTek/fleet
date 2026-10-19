"use client";

// Browser notifications preference row (Settings → General). The Web Push
// opt-in that used to live on the Connections page as NotificationsCard
// (#292), re-expressed as the design's preference row: label + description
// left, a switch right. Enabling walks the standard flow —
// Notification.requestPermission() → register /sw.js → subscribe with the
// server's VAPID public key (fetched, never build-time embedded) → store the
// subscription server-side. Disabling unsubscribes locally AND deletes the
// stored row. "On" means the server holds this browser's subscription for the
// signed-in user, not merely that the browser holds one; signing out lets the
// browser's subscription go (shared/signOut). The extra real-world states the
// design doesn't show (unsupported browser / operator hasn't configured VAPID
// / permission denied / a Viewer account) render as a note under the
// description with the switch disabled.

import { useEffect, useState } from "react";
import {
  ensureCurrentPushKey,
  lockedFetchSignal,
  sameKey,
  subscriptionKey,
  urlBase64ToUint8Array,
  withPushLock,
} from "@/app/shared/pushKey";
import { SetRow, SetSwitch } from "./ui/atoms";

type PushState =
  | "loading" // probing support + current subscription
  | "unsupported" // browser has no service worker / Push API
  | "unconfigured" // backend answered 501 — operator hasn't set VAPID keys
  | "denied" // the user blocked notifications for this site
  | "disabled" // supported + configured, not subscribed
  | "enabled"; // an active subscription is stored

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

// serverHoldsSubscription asks whether the signed-in user holds the stored
// row for this browser's subscription endpoint. Anything but a 200 throws, so
// a failed check is never read as "not yours". Called under the push lock.
async function serverHoldsSubscription(endpoint: string): Promise<boolean> {
  const res = await fetch("/api/push/status", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint }),
    cache: "no-store",
    signal: lockedFetchSignal(),
  });
  if (!res.ok) throw new Error(`Failed to check notifications: ${res.status}`);
  const data = (await res.json()) as { subscribed?: boolean };
  return data.subscribed === true;
}

// isViewerAccount reports whether the signed-in account is a Viewer, from the
// account read Settings → Team also makes (GET /api/me/team). The push routes
// refuse a Viewer's writes, so the switch is greyed out up front instead of
// failing once pressed. This is presentation only: the server's refusal stays
// the authority, and a failed read counts as "not a Viewer", leaving the
// switch to meet that refusal (which enable() turns into the same note).
async function isViewerAccount(): Promise<boolean> {
  try {
    const res = await fetch("/api/me/team", { cache: "no-store" });
    if (!res.ok) return false;
    const me = (await res.json()) as { role?: string };
    return me.role === "viewer";
  } catch {
    return false;
  }
}

// readOnlyRefusal reports whether a 403 is the server's Viewer refusal
// ({"error":"read_only"}) rather than another 403, such as the web app's
// origin check. It reads a clone, so the caller can still read the body.
async function readOnlyRefusal(res: Response): Promise<boolean> {
  if (res.status !== 403) return false;
  try {
    const { error } = (await res.clone().json()) as { error?: unknown };
    return error === "read_only";
  } catch {
    return false;
  }
}

// RENEWAL_PENDING explains a switch that reads off because this browser's
// renewal after a key change is still waiting on the server.
const RENEWAL_PENDING =
  "Notifications in this browser could not be renewed after a key change. Reload the page to try again.";

export function BrowserNotificationsRow() {
  const [state, setState] = useState<PushState>("loading");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // A Viewer account may not store a subscription. It is kept apart from
  // `state`, which still says what the browser and the server hold: the
  // checks below run for a Viewer too, so a subscription left in this browser
  // by an account that turned them on is still let go, and one the server
  // still holds for this account (from before it became a Viewer) still reads
  // on and can be turned off.
  const [viewer, setViewer] = useState(false);

  useEffect(() => {
    let stale = false;
    // Asked alongside the checks and applied with their result, so the switch
    // never offers a Viewer a moment in which to press it.
    const viewerRead = isViewerAccount();
    (async () => {
      if (!pushSupported()) return "unsupported" as const;
      const key = await fetchVapidKey();
      if (key === null) return "unconfigured" as const;
      if (Notification.permission === "denied") return "denied" as const;
      // A subscription made with an earlier VAPID key is renewed (or let go)
      // before the switch reads it; the same check the root layout runs,
      // shared while it is running, so the two never renew twice.
      const checked = await ensureCurrentPushKey();
      // Under the lock, like the renewal and enable(): another tab must not
      // replace the subscription between this read and the unsubscribe below.
      return withPushLock(async () => {
        const reg = await navigator.serviceWorker.getRegistration("/sw.js");
        const sub = await reg?.pushManager.getSubscription();
        if (!sub) return "disabled" as const;
        // A subscription in this browser counts only if the server holds it
        // for the signed-in user. It can be another account's (someone else
        // turned notifications on in this browser) or one the server has
        // dropped; either way nothing reaches this user through it, so the
        // switch reads off and the browser lets it go, which also stops
        // another account's alerts arriving here.
        if (await serverHoldsSubscription(sub.endpoint)) return "enabled" as const;
        // Except a renewal the server has not confirmed yet: the row has not
        // moved to this subscription, and letting it go would lose the retry.
        if (checked === "pending") throw new Error(RENEWAL_PENDING);
        await sub.unsubscribe().catch(() => undefined);
        return "disabled" as const;
      });
    })()
      .then(async (next) => {
        const isViewer = await viewerRead;
        if (stale) return;
        setViewer(isViewer);
        setState(next);
      })
      .catch(async (e: unknown) => {
        const isViewer = await viewerRead;
        if (stale) return;
        setViewer(isViewer);
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
      // Under the same lock as the key check, so another tab never replaces
      // the subscription between this read and the store.
      const { res, endpoint } = await withPushLock(async () => {
        let existing = await reg.pushManager.getSubscription();
        const existingKey = existing ? subscriptionKey(existing) : null;
        if (existing && existingKey && !sameKey(existingKey, key)) {
          // Made with an earlier VAPID key: it can receive nothing, and the
          // browser will not subscribe with the new key while it exists. Its
          // stored row (if it is this user's) goes too.
          await fetch("/api/push/unsubscribe", {
            method: "DELETE",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ endpoint: existing.endpoint }),
            signal: lockedFetchSignal(),
          });
          await existing.unsubscribe();
          existing = null;
        }
        const sub =
          existing ??
          (await reg.pushManager.subscribe({
            userVisibleOnly: true,
            applicationServerKey: urlBase64ToUint8Array(key).buffer as ArrayBuffer,
          }));
        const stored = await fetch("/api/push/subscribe", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(sub.toJSON()),
          signal: lockedFetchSignal(),
        });
        return { res: stored, endpoint: sub.endpoint };
      });
      if (res.status === 501) {
        setState("unconfigured");
        return;
      }
      if (await readOnlyRefusal(res)) {
        // A Viewer the account read missed (it failed, or the role changed
        // after this page loaded): the same note as one it caught.
        setViewer(true);
        setState("disabled");
        return;
      }
      if (!res.ok && res.status !== 204) {
        throw new Error((await res.text()) || `Subscribe failed: ${res.status}`);
      }
      // Another tab's Settings may have let this browser's subscription go
      // while it was being stored (it reads "not yours" until the store lands).
      if ((await reg.pushManager.getSubscription())?.endpoint !== endpoint) {
        throw new Error("Notifications were turned off in another tab. Turn them on again.");
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
        // drop the browser-side subscription. A Viewer's delete is refused,
        // but dropping the subscription still stops the alerts here, and the
        // server retires the row once the push service reports it gone.
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

  // What a press can do: turn off whatever reads on, and turn on only for an
  // account the server lets store a subscription.
  const canEnable = state === "disabled" && !viewer;

  // A Viewer is told first: allowing notifications in the browser, another
  // browser, or the operator setting push up would not let it turn them on.
  const note =
    viewer && state !== "enabled" ? (
      <span className="text-[var(--color-text-muted)]">
        Viewer accounts are read-only and can&apos;t turn notifications on — ask an admin if you
        need them.
      </span>
    ) : state === "unsupported" ? (
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
          Get alerts in this browser, even when this tab is in the background. By default you get
          one when a task you created finishes, is waiting for your answer, or sends a progress
          note while it runs; when an action in one of your chats needs your approval, and, for
          some actions, when an approved one finishes or the chat does not continue on its own
          once its card is approved, declined or timed out; and when you create a task while your
          spending is past your budget&apos;s soft limit. An alert shows only a short title, such
          as the start of the task&apos;s prompt, never the conversation or a result.
          {note ? <span className="mt-[0.3rem] block">{note}</span> : null}
        </>
      }
    >
      <SetSwitch
        on={state === "enabled"}
        onToggle={() => {
          if (busy) return;
          if (state === "enabled") void disable();
          else if (canEnable) void enable();
        }}
        disabled={busy || !(state === "enabled" || canEnable)}
        label="Browser notifications"
        testId="browser-notifications-switch"
      />
    </SetRow>
  );
}

export default BrowserNotificationsRow;
