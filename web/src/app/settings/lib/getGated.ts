// getGatedJson — the Settings pages' one way to load a session-gated JSON
// resource (#2007).
//
// Seven loaders (admin stats, users, providers, features, notifications,
// shared files, connections) each spelled out the same ladder: a 401 sends
// the page to /login (the session lapsed; there is nothing left to render),
// a few statuses get a sentence of their own ("You are not an admin."), and
// any other failure throws something for the page's error banner. They
// differed only in the words, and in whether the redirect was a
// location.href write or a location.assign call. This is that ladder once.
//
// A 401 answers null after starting the navigation, so the caller renders
// nothing more; a status named in `statusText` throws that sentence; any
// other non-2xx throws responseErrorText(res, "<label> request failed") —
// the server's reason when it gave one, "<label> request failed (NNN)" when
// it did not.

import { responseErrorText } from "@/app/lib/responseError";

export type GatedOptions = {
  /** Names the resource in the generic failure: "<label> request failed". */
  label: string;
  /** A sentence of its own for particular statuses (403, 501, 503, …). */
  statusText?: Record<number, string>;
};

/** Sends the page to sign-in: the session behind a gated read has lapsed. */
export function redirectToLogin(): void {
  window.location.assign("/login");
}

/** The failure for a non-2xx gated response (never a 401). */
export async function gatedFailure(res: Response, opts: GatedOptions): Promise<Error> {
  const own = opts.statusText?.[res.status];
  if (own) return new Error(own);
  return new Error(await responseErrorText(res, `${opts.label} request failed`));
}

export async function getGatedJson<T>(url: string, opts: GatedOptions): Promise<T | null> {
  const res = await fetch(url, { cache: "no-store" });
  if (res.status === 401) {
    redirectToLogin();
    return null;
  }
  if (!res.ok) throw await gatedFailure(res, opts);
  return (await res.json()) as T;
}
