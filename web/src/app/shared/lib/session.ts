"use client";

// The ONE client-side view of the signed-in user, shared by BOTH the chat and
// orchestrator views. It probes GET /api/session, which resolves whichever
// session cookie is present — the HMAC password cookie (elcano_session) OR the
// Ed25519 magic-link cookie (elcano_auth) — through the same getServerSession()
// the middleware gate uses (src/app/lib/auth.ts). So this is the authoritative
// "is there a valid cookie session, and for whom" check for the unified app.
//
// Before this module the two views each had their own probe: chat fetched
// /api/session inline in its boot effect, and the orchestrator hook hit a
// separate /api/orchestrator/me endpoint (since removed). Collapsing both onto
// /api/session means one endpoint (and one cookie-verification path) decides
// "signed in" everywhere.
//
// The orchestrator's moc username/password Bearer token is a separate, second
// credential layered ON TOP of this by useOrchestratorSession — it is not a
// cookie session and is intentionally not represented here.

export type ClientSession = {
  email: string;
};

// fetchClientSession resolves the current cookie session, or null when there is
// none (a 401, a non-OK status, or a network error). Callers redirect to /login
// on null. Kept as a plain async function — not a hook — so it can be called
// from imperative boot sequences (chat's mount-once effect) as well as from the
// orchestrator session hook.
export async function fetchClientSession(): Promise<ClientSession | null> {
  try {
    const res = await fetch("/api/session", { cache: "no-store" });
    if (!res.ok) return null;
    const data = (await res.json()) as { email?: string };
    if (!data.email) return null;
    return { email: data.email };
  } catch {
    return null;
  }
}
