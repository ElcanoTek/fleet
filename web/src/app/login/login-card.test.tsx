import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import LoginCard from "./login-card";
import { setBootstrapForTest } from "@/app/lib/bootstrap";


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

// page.tsx is the wiring: what it renders comes from the server-rendered
// bootstrap (internal/webui/spa.go) — the Elcano and SSO options derived from
// the same env the auth routes use, the bundle's login copy (#892), and the
// team-link flag (B24). The server-side halves — the silent SSO auto-start and
// AUTH_SIGNING_PUBKEY → magicLinkLoginEnabled — are pinned in
// internal/webui/spa_test.go.
describe("LoginPage — bootstrap wiring", () => {
  afterEach(() => {
    setBootstrapForTest(null);
    cleanup();
  });

  it("renders the card with both options when SSO is configured", async () => {
    setBootstrapForTest({ login: { passwordLoginEnabled: true, magicLinkLoginEnabled: false, oidcEnabled: true, oidcLabel: "Sign in with SSO", teamLink: false } });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("uses the operator's SSO button label", async () => {
    setBootstrapForTest({ login: { passwordLoginEnabled: true, magicLinkLoginEnabled: false, oidcEnabled: true, oidcLabel: "Sign in with Okta", teamLink: false } });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByRole("link", { name: "Sign in with Okta" })).toBeInTheDocument();
  });

  it("shows the Elcano button only when the server says the path is configured", async () => {
    setBootstrapForTest({ login: { passwordLoginEnabled: true, magicLinkLoginEnabled: true, oidcEnabled: false, oidcLabel: "Sign in with SSO", teamLink: false } });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByRole("link", { name: "Use Elcano email" })).toBeInTheDocument();
    cleanup();
    setBootstrapForTest({});
    render(<LoginPage />);
    expect(screen.queryByRole("link", { name: "Use Elcano email" })).toBeNull();
  });

  it("renders the bundle's login copy end to end", async () => {
    setBootstrapForTest({
      branding: {
        appName: "Reklaim",
        loginTitle: "Reklaim what's yours.",
        loginTagline: "Sign in and pick up where you left off.",
        shareTitle: "",
        shareDescription: "",
      },
    });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByText("Reklaim what's yours.")).toBeInTheDocument();
    expect(screen.getByText("Sign in and pick up where you left off.")).toBeInTheDocument();
  });

  // B24: the server sends a signed-out team-link visitor to /login?team_link=1.
  it("shows the team-link copy when the bootstrap says so and the bundle copy otherwise", async () => {
    setBootstrapForTest({ login: { passwordLoginEnabled: true, magicLinkLoginEnabled: false, oidcEnabled: false, oidcLabel: "Sign in with SSO", teamLink: true } });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByText("Sign in to open this chat")).toBeInTheDocument();
    cleanup();
    setBootstrapForTest({});
    render(<LoginPage />);
    expect(screen.queryByText("Sign in to open this chat")).toBeNull();
    expect(screen.getByText("Welcome aboard.")).toBeInTheDocument();
  });

  // The login page is the one page a locked-out operator must be able to
  // reach: with no bootstrap at all it still renders fleet's defaults.
  it("still renders with no bootstrap", async () => {
    setBootstrapForTest(null);
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByText("Welcome aboard.")).toBeInTheDocument();
  });
});

// A Fleet with central Auth SSO configured has no passwords of its own
// (FLEET_OIDC_ALLOW_PASSWORD is the break-glass): the card drops the form and
// its divider, SSO becomes the primary action, and no error tells the visitor
// to use a password the server would refuse. The server half is pinned in
// internal/webui/password_off_test.go.
describe("LoginCard: central Auth only", () => {
  afterEach(() => {
    setBootstrapForTest(null);
    cleanup();
    window.history.replaceState(null, "", "/login");
  });

  it("offers only SSO, as the primary action", () => {
    render(
      <LoginCard
        passwordLoginEnabled={false}
        magicLinkLoginEnabled={false}
        oidcEnabled
        oidcLabel="Sign in with Omnicom"
        {...DEFAULT_COPY}
      />,
    );
    const sso = screen.getByRole("link", { name: "Sign in with Omnicom" });
    expect(sso).toHaveAttribute("href", "/api/auth/oidc/start");
    expect(sso.className).toContain("--color-primary-fill");
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
    expect(screen.queryByLabelText("Password")).toBeNull();
    expect(screen.queryByLabelText("Email")).toBeNull();
    expect(screen.queryByText("or")).toBeNull();
    expect(document.querySelector('form[action="/api/auth/login"]')).toBeNull();
  });

  it("keeps the Elcano email button as the secondary option", () => {
    render(<LoginCard passwordLoginEnabled={false} magicLinkLoginEnabled oidcEnabled {...DEFAULT_COPY} />);
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Use Elcano email" })).toBeInTheDocument();
  });

  it.each([
    ["password_off", "Password sign-in is turned off here. Sign in with SSO."],
    ["invalid", "Password sign-in is turned off here. Sign in with SSO."],
    ["oidc_unavailable", "Single sign-on isn't available right now. Try again in a moment."],
    ["oidc_error", "Single sign-on failed. Try again."],
    ["elcano_unavailable", "Elcano email sign-in isn't available right now. Sign in with SSO."],
  ])("never points at a password for e=%s", async (code, message) => {
    window.history.replaceState(null, "", `/login?e=${code}`);
    render(<LoginCard passwordLoginEnabled={false} magicLinkLoginEnabled={false} oidcEnabled {...DEFAULT_COPY} />);
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.queryByText(/password\./i)).toBeNull();
  });

  it("takes the switch from the bootstrap", async () => {
    setBootstrapForTest({
      login: { passwordLoginEnabled: false, magicLinkLoginEnabled: false, oidcEnabled: true, oidcLabel: "Sign in with SSO", teamLink: false },
    });
    const { default: LoginPage } = await import("./page");
    render(<LoginPage />);
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
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
    // Load-bearing attributes the restyle must never lose.
    const form = screen
      .getByRole("button", { name: "Sign in" })
      .closest("form")!;
    expect(form.getAttribute("action")).toBe("/api/auth/login");
    expect(form.getAttribute("method")).toBe("post");
    const email = screen.getByLabelText("Email");
    expect(email).toHaveAttribute("name", "email");
    expect(email).toHaveAttribute("autocomplete", "email");
    expect(email).toBeRequired();
    const password = screen.getByLabelText("Password");
    expect(password).toHaveAttribute("name", "password");
    expect(password).toHaveAttribute("autocomplete", "current-password");
    expect(password).toBeRequired();
    expect(img?.getAttribute("alt")).toBe("");
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

// B24: a signed-out visitor who opened a team link is told why they are
// signing in and where they will land. The sign-in controls do not change.
describe("LoginCard — team link (B24)", () => {
  afterEach(cleanup);

  it("replaces the welcome copy with the team-link copy", () => {
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} teamLink />);
    expect(
      screen.getByRole("heading", { name: "Sign in to open this chat" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("You’ll go straight to the chat after you sign in."),
    ).toBeInTheDocument();
    expect(screen.queryByText(DEFAULT_COPY.title)).toBeNull();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("keeps the bundle copy without a team link", () => {
    render(<LoginCard magicLinkLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(screen.getByText(DEFAULT_COPY.title)).toBeInTheDocument();
    expect(screen.queryByText("Sign in to open this chat")).toBeNull();
  });
});
