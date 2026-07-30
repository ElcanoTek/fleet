import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import LoginCard from "./login-card";

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
};

describe("LoginCard — Elcano-email button gating", () => {
  afterEach(cleanup);

  it("renders the password form regardless of the Elcano-email gate", () => {
    render(<LoginCard elcanoLoginEnabled={false} {...DEFAULT_COPY} />);
    // Two password-path fields + the Sign in submit are always present.
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows neutral, client-agnostic welcome copy (no Elcano brand text)", () => {
    render(<LoginCard elcanoLoginEnabled={false} {...DEFAULT_COPY} />);
    expect(screen.getByText("Welcome aboard.")).toBeInTheDocument();
    expect(
      screen.getByText("Sign in to your workspace and pick up where you left off."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Elcano workspace/i)).toBeNull();
  });

  // The #892 regression: the card must render what it is given, not a literal.
  it("renders the bundle's login copy rather than a hardcoded default", () => {
    render(
      <LoginCard
        elcanoLoginEnabled={false}
        title="Reklaim what's yours."
        tagline="Sign in and pick up where you left off."
      />,
    );
    expect(screen.getByText("Reklaim what's yours.")).toBeInTheDocument();
    expect(screen.getByText("Sign in and pick up where you left off.")).toBeInTheDocument();
    expect(screen.queryByText("Welcome aboard.")).toBeNull();
  });

  it("shows the Use Elcano email button when enabled", () => {
    render(<LoginCard elcanoLoginEnabled={true} {...DEFAULT_COPY} />);
    const button = screen.getByRole("link", { name: "Use Elcano email" });
    expect(button).toHaveAttribute("href", "/api/auth/elcano-login");
    // The "or" divider only makes sense alongside the secondary path.
    expect(screen.getByText("or")).toBeInTheDocument();
  });

  it("omits the button and divider when disabled (white-label)", () => {
    render(<LoginCard elcanoLoginEnabled={false} {...DEFAULT_COPY} />);
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
        elcanoLoginEnabled={false}
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
    render(<LoginCard elcanoLoginEnabled={false} oidcEnabled={false} {...DEFAULT_COPY} />);
    expect(screen.queryByRole("link", { name: /sign in with/i })).toBeNull();
  });

  it("renders both secondary buttons when both paths are enabled", () => {
    render(
      <LoginCard elcanoLoginEnabled oidcEnabled oidcLabel="Sign in with SSO" {...DEFAULT_COPY} />,
    );
    expect(screen.getByRole("link", { name: "Sign in with SSO" })).toHaveAttribute(
      "href",
      "/api/auth/oidc/start",
    );
    expect(screen.getByRole("link", { name: "Use Elcano email" })).toHaveAttribute(
      "href",
      "/api/auth/elcano-login",
    );
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
    globalThis.fetch = originalFetch;
    cleanup();
    vi.resetModules();
  });

  function stubMeta(body: unknown, ok = true) {
    globalThis.fetch = vi.fn(async () => ({
      ok,
      json: async () => body,
    })) as unknown as typeof fetch;
  }

  it("passes elcanoLoginEnabled=true when AUTH_SIGNING_PUBKEY is set", async () => {
    process.env.AUTH_SIGNING_PUBKEY = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMzI=";
    stubMeta(null, false);
    const { default: LoginPage } = await import("./page");
    render(await LoginPage());
    expect(screen.getByRole("link", { name: "Use Elcano email" })).toBeInTheDocument();
  });

  it("passes elcanoLoginEnabled=false when AUTH_SIGNING_PUBKEY is unset", async () => {
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
    expect(screen.getByText("Sign in and pick up where you left off.")).toBeInTheDocument();
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
