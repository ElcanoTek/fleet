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
// subscription already on the current key, one renewed onto it, one that was
// not this user's to renew and was let go, a renewal the server did not
// confirm (the new subscription is kept and the renewal retried on the next
// check), or a browser that would not subscribe again.
export type PushKeyCheck = "none" | "current" | "renewed" | "dropped" | "pending" | "failed";

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

// PENDING_RENEWALS remembers, per browser, the endpoints renewals replaced
// when the server gave no answer to the move (offline, a timeout, a 5xx, an
// expired session). The new subscription is kept, and the next check retries
// the move from each of them. It is a set, not one endpoint: a move the
// server made whose answer was lost leaves the row at the endpoint that
// renewal made, and a later rotation must still be able to move it from
// there — so every unconfirmed endpoint is kept until the server answers for
// it, up to MAX_PENDING_RENEWALS (the most recent ones).
//
// The list lives under its own key. LEGACY_PENDING_RENEWAL is where the
// previous version kept a single endpoint, and a tab still running that
// bundle reads whatever is there as one endpoint: a list written there would
// be posted as old_endpoint, refused, and that tab would drop a valid
// subscription. So the legacy key is only ever read and removed, never
// written; an endpoint found there joins the list, oldest first.
const PENDING_RENEWALS = "fleet.push.pendingRenewals.v2";
const LEGACY_PENDING_RENEWAL = "fleet.push.pendingRenewal";
const MAX_PENDING_RENEWALS = 8;

// dedupe keeps the first of each non-empty endpoint, in order.
function dedupe(endpoints: (string | null | undefined)[]): string[] {
  return endpoints.filter((e, i, all): e is string => !!e && all.indexOf(e) === i);
}

function readPending(): string[] {
  let legacy: string | null;
  let raw: string | null;
  try {
    legacy = window.localStorage.getItem(LEGACY_PENDING_RENEWAL);
    raw = window.localStorage.getItem(PENDING_RENEWALS);
  } catch {
    return [];
  }
  let list: string[] = [];
  try {
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    if (Array.isArray(parsed)) list = parsed.filter((e): e is string => typeof e === "string");
  } catch {
    // Not ours to make sense of: start the list again.
  }
  return dedupe([...legacyEndpoints(legacy), ...list]);
}

// legacyEndpoints reads the legacy key: one endpoint, or — written there by a
// build of this change before the list moved to its own key — a list.
function legacyEndpoints(legacy: string | null): string[] {
  if (!legacy) return [];
  if (!legacy.startsWith("[")) return [legacy];
  try {
    const parsed: unknown = JSON.parse(legacy);
    return Array.isArray(parsed) ? parsed.filter((e): e is string => typeof e === "string") : [];
  } catch {
    return [];
  }
}

function writePending(endpoints: string[]): void {
  const keep = dedupe(endpoints).slice(-MAX_PENDING_RENEWALS);
  try {
    // Whatever was under the legacy key was read into endpoints first.
    window.localStorage.removeItem(LEGACY_PENDING_RENEWAL);
    if (keep.length) window.localStorage.setItem(PENDING_RENEWALS, JSON.stringify(keep));
    else window.localStorage.removeItem(PENDING_RENEWALS);
  } catch {
    // Without storage the renewal is simply not retried.
  }
}

// takePendingRenewal returns the endpoints of renewals the server never
// confirmed, and forgets them: sign-out deletes those rows too, since any of
// them may be the account's that is signing out.
export function takePendingRenewal(): string[] {
  const pending = readPending();
  writePending([]);
  return pending;
}

// LOCKED_FETCH_TIMEOUT_MS bounds each request made while holding the push
// lock, so one tab's hung request cannot hold every other tab's switch and
// renewal behind it.
const LOCKED_FETCH_TIMEOUT_MS = 10_000;

export function lockedFetchSignal(): AbortSignal | undefined {
  return typeof AbortSignal !== "undefined" && typeof AbortSignal.timeout === "function"
    ? AbortSignal.timeout(LOCKED_FETCH_TIMEOUT_MS)
    : undefined;
}

// withPushLock runs fn while holding a browser-wide lock, so two tabs (or the
// settings switch in one and the check in another) never replace the same
// subscription at once: unsubscribe acts on the browser's registration, not
// on the object a tab read earlier. Without the Web Locks API it just runs.
// With waitMs, fn runs without the lock if the lock is not free by then: for
// a caller that must finish on its own deadline (sign-out).
export async function withPushLock<T>(fn: () => Promise<T>, opts: { waitMs?: number } = {}): Promise<T> {
  const locks = typeof navigator !== "undefined" ? navigator.locks : undefined;
  if (!locks) return fn();
  if (opts.waitMs === undefined) return locks.request("fleet-push-subscription", fn);
  let granted = false;
  try {
    return await locks.request("fleet-push-subscription", { signal: AbortSignal.timeout(opts.waitMs) }, () => {
      granted = true;
      return fn();
    });
  } catch (e) {
    if (granted) throw e;
    return fn();
  }
}

let check: Promise<PushKeyCheck> | null = null;

// ensureCurrentPushKey checks this browser's subscription against the
// server's current key. Callers that ask while a check is running share it,
// so the root layout's check and the settings switch never renew twice; a
// later call checks again, so a tab left open across a rotation still
// notices. A check that throws counts as "none".
export function ensureCurrentPushKey(): Promise<PushKeyCheck> {
  check ??= withPushLock(checkPushKey)
    .catch(() => "none" as const)
    .finally(() => {
      check = null;
    });
  return check;
}

// askRenew asks the server to move this user's row from oldEndpoint to sub:
// true or false is the server's answer, null is none. A 400 is an answer — a
// request the server will never accept — and so is a 403 the server marks as
// its own refusal: {"error":"read_only"} for a Viewer account, which may not
// subscribe either, or {"error":"not_a_member"}. Both count as false. Any
// other 403 is not about this subscription — the web app's origin check
// ({"error":"forbidden"}, a misconfigured public origin) or a proxy in front
// of it — so, like every other failure (offline, a timeout, an expired
// session, a 5xx), it is no answer.
async function askRenew(oldEndpoint: string, sub: PushSubscription): Promise<boolean | null> {
  try {
    const res = await fetch("/api/push/renew", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ old_endpoint: oldEndpoint, ...sub.toJSON() }),
      signal: lockedFetchSignal(),
    });
    if (res.status === 400) return false;
    if (res.status === 403) return (await serverRefusal(res)) ? false : null;
    if (!res.ok) return null;
    return ((await res.json()) as { renewed?: boolean }).renewed === true;
  } catch {
    return null;
  }
}

// SERVER_REFUSALS are the 403 error codes the server sends for a caller it
// will never let renew.
const SERVER_REFUSALS = new Set(["read_only", "not_a_member"]);

async function serverRefusal(res: Response): Promise<boolean> {
  try {
    const { error } = (await res.json()) as { error?: unknown };
    return typeof error === "string" && SERVER_REFUSALS.has(error);
  } catch {
    return false;
  }
}

// renew moves this user's row to sub from the first of oldEndpoints the
// server confirms, and settles what the browser keeps: only an explicit "not
// yours" for every one lets the new subscription go. No answer keeps it and
// remembers every endpoint the server has not yet answered for — the one
// asked and the ones not reached — for the next check; an endpoint the
// server said no to is forgotten.
//
// A "not yours" is safe to act on even after a renewal whose answer was lost
// moved the row to sub itself: the server answers yes, not "not yours", when
// the caller already holds the new endpoint (RenewPushSubscription is
// idempotent), so a retry from the old endpoint confirms the row at sub
// rather than letting sub go.
async function renew(oldEndpoints: string[], sub: PushSubscription): Promise<PushKeyCheck> {
  for (const [i, oldEndpoint] of oldEndpoints.entries()) {
    const answer = await askRenew(oldEndpoint, sub);
    if (answer === null) {
      writePending(oldEndpoints.slice(i));
      return "pending";
    }
    if (answer) {
      writePending([]);
      return "renewed";
    }
  }
  writePending([]);
  // Not this user's subscription (another account's, on a shared browser):
  // let the new one go too, so the settings switch reads off and the user
  // can turn notifications on again.
  await sub.unsubscribe().catch(() => undefined);
  return "dropped";
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
  // Read inside the lock: another tab may have just replaced it.
  const reg = await navigator.serviceWorker.getRegistration("/sw.js");
  const sub = await reg?.pushManager.getSubscription();
  // A browser that does not report the key it subscribed with cannot be
  // checked; its subscription is left as it is.
  const known = sub ? subscriptionKey(sub) : null;
  if (!reg || !sub || !known) return "none";
  // Signed out, push not configured, or offline: nothing to compare against.
  const res = await fetch("/api/push/vapid-public-key", { cache: "no-store", signal: lockedFetchSignal() });
  if (!res.ok) return "none";
  const { key } = (await res.json()) as { key?: string };
  if (!key) return "none";
  if (sameKey(known, key)) {
    // On the current key, but a renewal the server never confirmed is
    // retried now.
    const pending = readPending().filter((e) => e !== sub.endpoint);
    if (pending.length) return renew(pending, sub);
    writePending([]);
    return "current";
  }

  // Made with an earlier key. The browser will not subscribe with a new key
  // while it holds one made with another, and the old one can no longer
  // receive anything, so it goes first.
  await sub.unsubscribe();
  let fresh: PushSubscription;
  try {
    fresh = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(key).buffer as ArrayBuffer,
    });
  } catch {
    // The old subscription is gone and the browser refused a new one: the
    // settings switch now reads off, and turning it on tries again.
    return "failed";
  }
  // Renewals still pending from earlier rotations mean the user's row may
  // still sit at one of those endpoints rather than this one: try them first.
  return renew(dedupe([...readPending(), sub.endpoint]), fresh);
}
