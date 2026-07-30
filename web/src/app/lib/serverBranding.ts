import { getChatServerBase, getSharedToken } from "@/app/lib/chatServer";

/**
 * serverBranding — the one server-side reader of the deployment's white-label
 * identity, for the surfaces that CANNOT use /client-config.
 *
 * /client-config is member-gated. Three surfaces need branding without a
 * session, and each one silently fell back to a fleet default because of it:
 *
 *   - the login card, which renders pre-auth (#892: `login_title` was hardcoded)
 *   - the root layout's title and its og / twitter tags, read by anonymous
 *     unfurl scrapers (#894: `share_title` was read by zero components)
 *   - <meta name="theme-color"> and the PWA manifest (#895: fleet purple
 *     literals)
 *
 * They read chat-server's token-gated, identity-less /brand/meta instead — the
 * same trust class as /theme.css and /brand/logo, which exist for exactly this
 * reason. Every field is public by construction: the app name is in the browser
 * tab, the login copy is printed on the pre-auth page, and the share strings go
 * into OG tags anonymous scrapers read.
 *
 * ## Failure is always cosmetic
 *
 * Branding must never be able to fail a page render. Every error path — backend
 * down, no bundle, malformed JSON, slow response — returns DEFAULTS, which mirror
 * `clientconfig.applyBrandingDefaults` so a no-bundle deployment is coherent.
 *
 * ## Two traps this deliberately avoids
 *
 * **Build-time execution.** `manifest.ts` was written to resolve the palette at
 * request time and never did: its route was statically prerendered, so the fetch
 * ran during `next build` — in a staging dir with the backend down (see
 * scripts/update.sh) — and took the fallback on every single deployment. Every
 * consumer of this helper must therefore be dynamic (`export const dynamic =
 * "force-dynamic"`, or a `generateMetadata`/`generateViewport` in a dynamic
 * route). A silent build-time fallback is invisible until you diff two live
 * endpoints, so `serverBranding.test.ts` pins the consumers' dynamic flags.
 *
 * **A hung backend blocking HTML.** This runs on the critical path of every
 * page render, so the fetch carries an explicit timeout; without one, a
 * chat-server that accepts the connection but never answers would hang metadata
 * generation rather than degrading to defaults.
 */

export type ServerBranding = {
  appName: string;
  loginTitle: string;
  loginTagline: string;
  shareTitle: string;
  shareDescription: string;
  /** The bundle's `background` token per mode, or null to use the web's own. */
  backgroundLight: string | null;
  backgroundDark: string | null;
};

/**
 * Mirrors `clientconfig.applyBrandingDefaults` (internal/clientconfig) and
 * `useClientConfig.ts`'s client-side defaults, so no-bundle, sparse-bundle, and
 * backend-unreachable all render the same coherent identity.
 *
 * `NEXT_PUBLIC_APP_NAME` remains the app-name fallback for one narrow reason: it
 * is the only value available if the backend is unreachable, and an operator who
 * set it should not see "Fleet" when that happens. The bundle wins when reachable,
 * which is the fix for #894 — the env var is no longer the primary source.
 */
export const BRANDING_DEFAULTS: ServerBranding = {
  appName: process.env.NEXT_PUBLIC_APP_NAME?.trim() || "Fleet",
  loginTitle: "Welcome aboard.",
  loginTagline: "Sign in to your workspace and pick up where you left off.",
  shareTitle: "",
  shareDescription: "An AI workspace with real tool use.",
  backgroundLight: null,
  backgroundDark: null,
};

/**
 * Fleet's own `--color-bg` per theme, mirroring globals.css. Used when the bundle
 * declares no background for a mode, so browser chrome still matches the shell.
 * Keep in sync with the `:root` / `:root[data-theme="light"]` blocks.
 */
export const DEFAULT_BACKGROUND_DARK = "#1a0b1e";
export const DEFAULT_BACKGROUND_LIGHT = "#f4f6fb";

/**
 * How long a resolved payload is reused within this process. The bundle is read
 * at boot and only changes across a restart, so the sole purpose of a short TTL
 * is to avoid a per-request round trip on the HTML critical path while still
 * picking up an operator restart promptly. Mirrors the route's own max-age.
 */
const CACHE_TTL_MS = 60_000;

/** Guard against a backend that accepts the connection but never responds. */
const FETCH_TIMEOUT_MS = 2_000;

let cache: { at: number; value: ServerBranding } | null = null;

/** Test seam: drop the memo so a test can observe a second fetch. */
export function __resetBrandingCache(): void {
  cache = null;
}

function str(v: unknown, fallback: string): string {
  return typeof v === "string" && v.trim() !== "" ? v : fallback;
}

function color(v: unknown): string | null {
  return typeof v === "string" && v.trim() !== "" ? v.trim() : null;
}

/**
 * getServerBranding resolves the deployment's identity, memoized for CACHE_TTL_MS.
 * Never throws and never rejects: callers can use the result unconditionally.
 */
export async function getServerBranding(): Promise<ServerBranding> {
  const now = Date.now();
  if (cache && now - cache.at < CACHE_TTL_MS) return cache.value;

  let value = BRANDING_DEFAULTS;
  try {
    const res = await fetch(`${getChatServerBase()}/brand/meta`, {
      headers: { "X-Chat-Server-Token": getSharedToken() },
      cache: "no-store",
      signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
    });
    if (res.ok) {
      const raw: unknown = await res.json();
      if (raw && typeof raw === "object") {
        const d = raw as Record<string, unknown>;
        const appName = str(d.app_name, BRANDING_DEFAULTS.appName);
        value = {
          appName,
          loginTitle: str(d.login_title, BRANDING_DEFAULTS.loginTitle),
          loginTagline: str(d.login_tagline, BRANDING_DEFAULTS.loginTagline),
          // The backend already defaults share_title to app_name; mirror that
          // here so a hand-rolled/partial payload still yields something usable.
          shareTitle: str(d.share_title, appName),
          shareDescription: str(d.share_description, BRANDING_DEFAULTS.shareDescription),
          backgroundLight: color(d.background_light),
          backgroundDark: color(d.background_dark),
        };
      }
    }
  } catch {
    // Backend unreachable, timed out, or returned unparseable JSON. Branding is
    // cosmetic — fall back rather than fail the render.
  }

  // Cached even on the fallback path, so a down backend cannot turn every page
  // render into a fresh 2s timeout.
  cache = { at: now, value };
  return value;
}
