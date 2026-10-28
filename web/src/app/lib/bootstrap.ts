// The per-request state the Go server renders into index.html
// (<script type="application/json" id="fleet-bootstrap">, see
// internal/webui/spa.go Bootstrap). It replaces what the Next.js server
// components computed before rendering: the deployment's branding, the signed-in
// email, the login card's options, a shared conversation's snapshot.
//
// It is data, never code: a JSON <script> is not executed, so the strict CSP
// (script-src 'self') holds.

import type { SharedSnapshot } from "@/app/shared/[token]/SharedConversationView";

export type BootBranding = {
  appName: string;
  loginTitle: string;
  loginTagline: string;
  shareTitle: string;
  shareDescription: string;
  backgroundLight?: string;
  backgroundDark?: string;
  shareImageUrl?: string;
};

export type Bootstrap = {
  buildId: string;
  branding: BootBranding;
  session: { email: string } | null;
  login: {
    /** False on a Fleet that signs in through central Auth only (Go PasswordLoginEnabled). */
    passwordLoginEnabled: boolean;
    magicLinkLoginEnabled: boolean;
    oidcEnabled: boolean;
    oidcLabel: string;
    teamLink: boolean;
  };
  shared?: SharedSnapshot;
  notFound?: boolean;
  /** Routes whose page the server decides (Go fullLoadRoutes); "/x/" is a prefix. */
  fullLoadRoutes?: string[];
  /**
   * Present when central Auth manages this Fleet's accounts, roles and teams
   * (Go bootCentralAuth): the account pages are view-only and link to Auth.
   */
  centralAuth?: { adminUrl: string };
};

/** Mirrors the Go DefaultBranding (and clientconfig's defaults). */
export const DEFAULT_BOOTSTRAP: Bootstrap = {
  buildId: "dev",
  branding: {
    appName: "Fleet",
    loginTitle: "Welcome aboard.",
    loginTagline: "Sign in to your workspace and pick up where you left off.",
    shareTitle: "",
    shareDescription: "An AI workspace with real tool use.",
  },
  session: null,
  login: {
    passwordLoginEnabled: true,
    magicLinkLoginEnabled: false,
    oidcEnabled: false,
    oidcLabel: "Sign in with SSO",
    teamLink: false,
  },
  fullLoadRoutes: ["/login", "/no-access", "/shared/"],
};

let cached: Bootstrap | null = null;

/** getBootstrap reads (once) the server-rendered bootstrap, or the defaults. */
export function getBootstrap(): Bootstrap {
  if (cached) return cached;
  let parsed: Partial<Bootstrap> = {};
  if (typeof document !== "undefined") {
    const el = document.getElementById("fleet-bootstrap");
    if (el?.textContent) {
      try {
        parsed = JSON.parse(el.textContent) as Partial<Bootstrap>;
      } catch {
        parsed = {};
      }
    }
  }
  cached = {
    ...DEFAULT_BOOTSTRAP,
    ...parsed,
    branding: { ...DEFAULT_BOOTSTRAP.branding, ...(parsed.branding ?? {}) },
    login: { ...DEFAULT_BOOTSTRAP.login, ...(parsed.login ?? {}) },
    session: parsed.session ?? null,
  };
  return cached;
}

/** Test seam: install a bootstrap (or reset to the page's on null). */
export function setBootstrapForTest(b: Partial<Bootstrap> | null): void {
  cached = b
    ? {
        ...DEFAULT_BOOTSTRAP,
        ...b,
        branding: { ...DEFAULT_BOOTSTRAP.branding, ...(b.branding ?? {}) },
        login: { ...DEFAULT_BOOTSTRAP.login, ...(b.login ?? {}) },
        session: b.session ?? null,
      }
    : null;
}
