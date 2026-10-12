// Keeps this browser's Web Push subscription (#292) on the server's current
// VAPID key. A subscription is bound to the key it was made with, so after the
// operator rotates FLEET_VAPID_* every existing one is dead: the push service
// refuses each send the server signs with the new key, and nothing tells the
// browser. The subscription records its key, so the browser can notice and
// renew: subscribe again with the current key, then ask the server to move
// the signed-in user's row to the new subscription (POST /api/push/renew).
// The server moves it only when that user held the old row, so a browser
// holding another account's subscription never signs up whoever is signed in
// now; the new subscription is dropped instead.

// What the check found: no subscription (or nothing to compare against), a
// subscription already on the current key, one renewed onto it, or one that
// could not be renewed for this user and was let go.
export type PushKeyCheck = "none" | "current" | "renewed" | "dropped";

// urlBase64ToUint8Array decodes a base64url VAPID public key into the
// BufferSource shape pushManager.subscribe expects.
export function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padding = "=".repeat((4 - (base64.length % 4)) % 4);
  const raw = window.atob((base64 + padding).replace(/-/g, "+").replace(/_/g, "/"));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function toBase64Url(bytes: ArrayBuffer): string {
  let raw = "";
  for (const b of new Uint8Array(bytes)) raw += String.fromCharCode(b);
  return window.btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// subscriptionKey is the base64url VAPID key a subscription was made with, or
// null when the browser does not report it.
export function subscriptionKey(sub: PushSubscription): string | null {
  const key = sub.options?.applicationServerKey;
  return key ? toBase64Url(key) : null;
}

// sameKey compares two base64url keys, ignoring padding and alphabet variants.
export function sameKey(a: string, b: string): boolean {
  const norm = (k: string) => k.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return norm(a) === norm(b);
}

let check: Promise<PushKeyCheck> | null = null;

// ensureCurrentPushKey runs the check once per page load and hands every
// caller the same promise, so the root layout's renewal and the settings
// switch never renew the same subscription twice. A failed check counts as
// "none": it changes nothing the next page load cannot retry.
export function ensureCurrentPushKey(): Promise<PushKeyCheck> {
  check ??= checkPushKey().catch(() => "none" as const);
  return check;
}

async function checkPushKey(): Promise<PushKeyCheck> {
  if (
    typeof window === "undefined" ||
    !("serviceWorker" in navigator) ||
    !("PushManager" in window) ||
    !("Notification" in window) ||
    Notification.permission !== "granted"
  ) {
    return "none";
  }
  const reg = await navigator.serviceWorker.getRegistration("/sw.js");
  const sub = await reg?.pushManager.getSubscription();
  const known = sub ? subscriptionKey(sub) : null;
  if (!reg || !sub || !known) return "none";
  // Signed out, push not configured, or offline: nothing to compare against.
  const res = await fetch("/api/push/vapid-public-key", { cache: "no-store" });
  if (!res.ok) return "none";
  const { key } = (await res.json()) as { key?: string };
  if (!key) return "none";
  if (sameKey(known, key)) return "current";

  // Made with an earlier key. The browser will not subscribe with a new key
  // while it holds one made with another, and the old one can no longer
  // receive anything, so it goes first.
  await sub.unsubscribe();
  const fresh = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(key).buffer as ArrayBuffer,
  });
  const renew = await fetch("/api/push/renew", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ old_endpoint: sub.endpoint, ...fresh.toJSON() }),
  }).catch(() => null);
  const renewed =
    !!renew?.ok && ((await renew.json()) as { renewed?: boolean }).renewed === true;
  if (renewed) return "renewed";
  // Not this user's subscription (another account's, on a shared browser), or
  // the server could not take the move: let the new one go too, so the
  // settings switch reads off and the user can turn notifications on again.
  await fresh.unsubscribe().catch(() => undefined);
  return "dropped";
}
