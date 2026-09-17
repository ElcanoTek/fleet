import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import LoginCard from "./login-card";

// next/navigation's redirect() throws a framework-internal signal; stand in a
// throw we can assert on so the auto-start tests below see the target URL.
vi.mock("next/navigation", () => ({
  redirect: vi.fn((url: string) => {
    throw new Error(`REDIRECT:${url}`);
  }),
}));

// The "Use Elcano email" button is the only visible surface of the Elcano
// magic-link path. White-labelled deploys leave AUTH_SIGNING_PUBKEY unset, and
// the card must then show *only* the password form — no Elcano-branded button,
// no "or" divider. These tests pin that gating so a refactor can't silently
// leak the button onto a relabelled login page.

// The card's copy arrives as props now (#892). It used to be hardcoded, so a
// bundle's branding.login_title never rendered; the defaults below stand in for
// what the page-level server component resolves from /brand/meta.
const DEFAULT_COPY = {
  title: "Welcome aboard.",
  tagline: "Sign in to your workspace and pick up where you left off.",
  appName: "Fleet",
};

describe("LoginCard — Elcano-email button gating", () => {
  afterEach(cleanup);

  it("renders the password form regardless of the Elcano-email gate", () => {
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    // Two password-path fields + the Sign in submit are always present.
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows neutral, client-agnostic welcome copy (no Elcano brand text)", () => {
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(screen.getByText("Welcome aboard.")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Sign in to your workspace and pick up where you left off.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Elcano workspace/i)).toBeNull();
  });

  // The #892 regression: the card must render what it is given, not a literal.
  it("renders the bundle's login copy rather than a hardcoded default", () => {
    render(
      <LoginCard
        magicLinkLoginEnabled={false}
        title="Reklaim what's yours."
        tagline="Sign in and pick up where you left off."
        appName="Fleet"
      />,
    );
    expect(screen.getByText("Reklaim what's yours.")).toBeInTheDocument();
    expect(
      screen.getByText("Sign in and pick up where you left off."),
    ).toBeInTheDocument();
    expect(screen.queryByText("Welcome aboard.")).toBeNull();
  });

  it("shows the Use Elcano email button when enabled", () => {
    render(<LoginCard magicLinkLoginEnabled={true} {...DEFAULT_COPY} />);
    const button = screen.getByRole("link", { name: "Use Elcano email" });
    expect(button).toHaveAttribute("href", "/api/auth/elcano-login");
    // The "or" divider only makes sense alongside the secondary path.
    expect(screen.getByText("or")).toBeInTheDocument();
  });

  it("omits the button and divider when disabled (white-label)", () => {
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(screen.queryByRole("link", { name: "Use Elcano email" })).toBeNull();
    expect(screen.queryByText("or")).toBeNull();
  });
});

// The SSO button (#240) is the only visible surface of the OIDC path. It is
// gated independently of the Elcano-email button, uses the operator-chosen
// label, and points at the /start leg of the flow.
describe("LoginCard — OIDC SSO button gating", () => {
  afterEach(cleanup);

  it("shows the SSO button with the operator label when enabled", () => {
    render(
      <LoginCard
        magicLinkLoginEnabled={false}
        oidcEnabled
        oidcLabel="Sign in with Okta"
        {...DEFAULT_COPY}
      />,
    );
    const button = screen.getByRole("link", { name: "Sign in with Okta" });
    expect(button).toHaveAttribute("href", "/api/auth/oidc/start");
    // The divider shows even when only the SSO path is enabled.
    expect(screen.getByText("or")).toBeInTheDocument();
  });

  it("omits the SSO button when disabled", () => {
    render(
      <LoginCard
        magicLinkLoginEnabled={false}
        oidcEnabled={false}
        {...DEFAULT_COPY}
      />,
    );
    expect(screen.queryByRole("link", { name: /sign in with/i })).toBeNull();
  });

  it("renders both secondary buttons when both paths are enabled", () => {
    render(
      <LoginCard
        magicLinkLoginEnabled
        oidcEnabled
        oidcLabel="Sign in with SSO"
        {...DEFAULT_COPY}
      />,
    );
    expect(
      screen.getByRole("link", { name: "Sign in with SSO" }),
    ).toHaveAttribute("href", "/api/auth/oidc/start");
    expect(
      screen.getByRole("link", { name: "Use Elcano email" }),
    ).toHaveAttribute("href", "/api/auth/elcano-login");
  });
});

// The ?e= codes are the only channel the POST /api/auth/login redirect has to
// explain a failure. "throttled" (verify endpoint's 429) must render the
// throttle copy — it used to fall into the generic could-not-sign-in bucket
// via the "server" code, telling rate-limited users the server was down.
describe("LoginCard — ?e= error rendering", () => {
  afterEach(() => {
    cleanup();
    window.history.replaceState(null, "", "/login");
  });

  it("renders the throttle message for e=throttled", async () => {
    window.history.replaceState(null, "", "/login?e=throttled");
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(
      await screen.findByText(
        "Too many sign-in attempts. Try again in a minute.",
      ),
    ).toBeInTheDocument();
  });

  it("renders the unreachable message for e=server", async () => {
    window.history.replaceState(null, "", "/login?e=server");
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(
      await screen.findByText(
        "The chat server isn't reachable right now. Try again in a moment.",
      ),
    ).toBeInTheDocument();
  });
});

// page.tsx is the wiring: it must derive the Elcano prop from AUTH_SIGNING_PUBKEY
// — the same gate the elcano-login backend route uses — so the UI and the
// redirect handler can never disagree about whether the path is live. It is also
// where the bundle's login copy is resolved (#892), since the card is a client
// component and /client-config is member-gated.
describe("LoginPage — server-side wiring", () => {
  const originalFetch = globalThis.fetch;

  beforeEach(() => {
    process.env.CHAT_SERVER_URL = "http://127.0.0.1:8080";
    process.env.CHAT_SERVER_TOKEN = "test-token";
    vi.resetModules();
  });

  afterEach(() => {
    delete process.env.AUTH_SIGNING_PUBKEY;
    for (const key of Object.keys(process.env))
      if (key.startsWith("FLEET_OIDC_")) delete process.env[key];
    globalThis.fetch = originalFetch;
    cleanup();
    vi.resetModules();
  });

  function enableOidc(autoStart: boolean) {
    process.env.FLEET_OIDC_ISSUER = "https://idp.example.com";
    process.env.FLEET_OIDC_CLIENT_ID = "client-123";
    process.env.FLEET_OIDC_CLIENT_SECRET = "secret-xyz";
    process.env.FLEET_OIDC_AUTO_START = autoStart ? "1" : "";
  }

  it("auto-starts a silent SSO attempt on a plain visit when FLEET_OIDC_AUTO_START is set", async () => {
    enableOidc(true);
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    await expect(
      LoginPage({ searchParams: Promise.resolve({}) }),
    ).rejects.toThrow("REDIRECT:/api/auth/oidc/start?silent=1");
  });

  it("renders the card with both options after a silent attempt found no session, and for ?manual / ?e", async () => {
    enableOidc(true);
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    for (const query of [{ sso: "none" }, { manual: "1" }, { e: "invalid" }]) {
      cleanup();
      render(await LoginPage({ searchParams: Promise.resolve(query) }));
      expect(
        screen.getByRole("link", { name: "Sign in with SSO" }),
      ).toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: "Sign in" }),
      ).toBeInTheDocument();
    }
  });

  it("never auto-starts when the flag is off, even with OIDC configured", async () => {
    enableOidc(false);
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    render(await LoginPage({ searchParams: Promise.resolve({}) }));
    expect(
      screen.getByRole("link", { name: "Sign in with SSO" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  function stubMeta(body: unknown, ok = true) {
    globalThis.fetch = vi.fn(async () => ({
      ok,
      json: async () => body,
    })) as unknown as typeof fetch;
  }

  it("passes magicLinkLoginEnabled=true when AUTH_SIGNING_PUBKEY is set", async () => {
    process.env.AUTH_SIGNING_PUBKEY = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMzI=";
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    render(await LoginPage());
    expect(
      screen.getByRole("link", { name: "Use Elcano email" }),
    ).toBeInTheDocument();
  });

  it("passes magicLinkLoginEnabled=false when AUTH_SIGNING_PUBKEY is unset", async () => {
    delete process.env.AUTH_SIGNING_PUBKEY;
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    render(await LoginPage());
    expect(screen.queryByRole("link", { name: "Use Elcano email" })).toBeNull();
  });

  it("renders the bundle's login copy end to end", async () => {
    stubMeta({
      app_name: "Reklaim",
      login_title: "Reklaim what's yours.",
      login_tagline: "Sign in and pick up where you left off.",
    });
    const { default: LoginPage } = await import("./page");
    render(await LoginPage());
    expect(screen.getByText("Reklaim what's yours.")).toBeInTheDocument();
    expect(
      screen.getByText("Sign in and pick up where you left off."),
    ).toBeInTheDocument();
  });

  // The login page is the one page a locked-out operator must be able to reach.
  it("still renders when the backend is unreachable", async () => {
    globalThis.fetch = vi.fn(async () => {
      throw new Error("ECONNREFUSED");
    }) as unknown as typeof fetch;
    const { default: LoginPage } = await import("./page");
    render(await LoginPage());
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByText("Welcome aboard.")).toBeInTheDocument();
  });
});

// The card is Auth's sign-in card (auth/internal/httpapi/templates.go): mark,
// wordmark, title, tagline, labelled fields, one primary action, secondary
// sign-ins under a divider, footer note. These pin the anatomy so the two
// pages cannot drift apart again.
describe("LoginCard — matches Auth's sign-in card", () => {
  afterEach(cleanup);

  it("renders the bundle mark and the wordmark above the title", () => {
    render(
      <LoginCard
        magicLinkLoginEnabled={false}
        oidcEnabled
        oidcLabel="Sign in with Omnicom SSO"
        {...DEFAULT_COPY}
        appName="OMNICOM"
      />,
    );
    const img = document.querySelector("img");
    expect(img?.getAttribute("src")).toBe("/api/brand/logo");
    expect(screen.getByText("OMNICOM")).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: "Welcome aboard." }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Email")).toHaveAttribute("type", "email");
    expect(screen.getByLabelText("Password")).toHaveAttribute(
      "type",
      "password",
    );
    expect(screen.getByRole("button", { name: "Sign in" })).toHaveAttribute(
      "type",
      "submit",
    );
    expect(
      screen.getByRole("link", { name: "Sign in with Omnicom SSO" }),
    ).toHaveAttribute("href", "/api/auth/oidc/start");
    expect(
      screen.getByText("Accounts are created by an administrator."),
    ).toBeInTheDocument();
  });

  it("shows the sign-in error in an alert like Auth's", async () => {
    window.history.replaceState(null, "", "/login?e=invalid");
    try {
      render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Invalid email or password.",
      );
    } finally {
      window.history.replaceState(null, "", "/login");
    }
  });
});
