import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  __resetDiscoveryCacheForTest,
  buildRedirectUri,
  decodeJwtClaims,
  discover,
  emailDomainAllowed,
  getOidcConfig,
  oidcEnabled,
  pkceChallenge,
  randomUrlSafe,
  validateIdToken,
  type DiscoveryDoc,
  type OidcConfig,
} from "./oidc";
import { NextRequest } from "next/server";

const FULL_ENV = {
  FLEET_OIDC_ISSUER: "https://idp.example.com",
  FLEET_OIDC_CLIENT_ID: "client-123",
  FLEET_OIDC_CLIENT_SECRET: "secret-xyz",
};

function jwt(claims: Record<string, unknown>): string {
  const b64 = (o: unknown) => Buffer.from(JSON.stringify(o)).toString("base64url");
  return `${b64({ alg: "RS256" })}.${b64(claims)}.signature-not-verified`;
}

const discovery: DiscoveryDoc = {
  issuer: "https://idp.example.com",
  authorization_endpoint: "https://idp.example.com/authorize",
  token_endpoint: "https://idp.example.com/token",
};

describe("getOidcConfig / oidcEnabled", () => {
  const original = process.env;
  beforeEach(() => {
    process.env = { ...original };
  });
  afterEach(() => {
    process.env = original;
  });

  it("returns null (disabled) when any load-bearing var is missing", () => {
    expect(getOidcConfig()).toBeNull();
    process.env.FLEET_OIDC_ISSUER = FULL_ENV.FLEET_OIDC_ISSUER;
    process.env.FLEET_OIDC_CLIENT_ID = FULL_ENV.FLEET_OIDC_CLIENT_ID;
    // secret still missing
    expect(getOidcConfig()).toBeNull();
    expect(oidcEnabled()).toBe(false);
  });

  it("parses a full config with sensible defaults", () => {
    Object.assign(process.env, FULL_ENV);
    const c = getOidcConfig()!;
    expect(c).not.toBeNull();
    expect(c.issuer).toBe("https://idp.example.com");
    expect(c.scopes).toBe("openid email profile");
    expect(c.buttonLabel).toBe("Sign in with SSO");
    expect(c.allowedDomains).toEqual([]);
    expect(oidcEnabled()).toBe(true);
  });

  it("trims a trailing slash off the issuer and normalizes the domain allowlist", () => {
    Object.assign(process.env, FULL_ENV);
    process.env.FLEET_OIDC_ISSUER = "https://idp.example.com/";
    process.env.FLEET_OIDC_ALLOWED_DOMAINS = " @Example.com, foo.io ,, ";
    process.env.FLEET_OIDC_BUTTON_LABEL = "Log in with Okta";
    const c = getOidcConfig()!;
    expect(c.issuer).toBe("https://idp.example.com");
    expect(c.allowedDomains).toEqual(["example.com", "foo.io"]);
    expect(c.buttonLabel).toBe("Log in with Okta");
  });
});

describe("emailDomainAllowed", () => {
  it("admits any domain when the allowlist is empty", () => {
    expect(emailDomainAllowed("anyone@whatever.com", [])).toBe(true);
  });
  it("admits only listed domains, case-insensitively", () => {
    expect(emailDomainAllowed("a@Example.com", ["example.com"])).toBe(true);
    expect(emailDomainAllowed("a@evil.com", ["example.com"])).toBe(false);
  });
  it("rejects a malformed email", () => {
    expect(emailDomainAllowed("not-an-email", ["example.com"])).toBe(false);
  });
});

describe("decodeJwtClaims", () => {
  it("decodes the payload of a well-formed JWT", () => {
    const claims = decodeJwtClaims(jwt({ email: "x@y.com", sub: "1" }));
    expect(claims).toMatchObject({ email: "x@y.com", sub: "1" });
  });
  it("returns null for a non-JWT", () => {
    expect(decodeJwtClaims("garbage")).toBeNull();
    expect(decodeJwtClaims("a.b")).toBeNull();
  });
});

describe("validateIdToken", () => {
  const config: OidcConfig = {
    issuer: "https://idp.example.com",
    clientId: "client-123",
    clientSecret: "secret-xyz",
    scopes: "openid email",
    allowedDomains: [],
    buttonLabel: "SSO",
  };
  const now = 1_000_000;
  const base = {
    iss: "https://idp.example.com",
    aud: "client-123",
    exp: now + 600,
    nonce: "nonce-abc",
    email: "User@Example.com",
  };

  it("accepts a valid token and lowercases the email", () => {
    const v = validateIdToken(base, config, discovery, "nonce-abc", now);
    expect(v).toEqual({ ok: true, email: "user@example.com" });
  });

  it("accepts an array audience that contains the client id", () => {
    const v = validateIdToken({ ...base, aud: ["other", "client-123"] }, config, discovery, "nonce-abc", now);
    expect(v.ok).toBe(true);
  });

  it.each([
    ["issuer mismatch", { iss: "https://evil.com" }],
    ["audience mismatch", { aud: "someone-else" }],
    ["azp mismatch", { azp: "someone-else" }],
    ["expired", { exp: now - 1 }],
    ["nonce mismatch", { nonce: "different" }],
    ["no email claim", { email: undefined }],
    ["email not verified", { email_verified: false }],
  ])("rejects on %s", (_label, override) => {
    const v = validateIdToken({ ...base, ...override }, config, discovery, "nonce-abc", now);
    expect(v.ok).toBe(false);
  });

  it("rejects an unparseable token", () => {
    expect(validateIdToken(null, config, discovery, "nonce-abc", now).ok).toBe(false);
  });
});

describe("discover", () => {
  beforeEach(() => __resetDiscoveryCacheForTest());

  it("fetches + caches the well-known document", async () => {
    const fetchImpl = vi.fn(async () =>
      new Response(JSON.stringify(discovery), { status: 200, headers: { "Content-Type": "application/json" } }),
    ) as unknown as typeof fetch;
    const a = await discover("https://idp.example.com", fetchImpl);
    const b = await discover("https://idp.example.com", fetchImpl);
    expect(a.token_endpoint).toBe("https://idp.example.com/token");
    expect(b).toBe(a);
    expect(fetchImpl).toHaveBeenCalledTimes(1); // cached
    expect(fetchImpl).toHaveBeenCalledWith(
      "https://idp.example.com/.well-known/openid-configuration",
      expect.anything(),
    );
  });

  it("throws (and does not cache) on a bad response", async () => {
    const bad = vi.fn(async () => new Response("nope", { status: 500 })) as unknown as typeof fetch;
    await expect(discover("https://idp.example.com", bad)).rejects.toThrow();
    // A later good fetch succeeds — the failure wasn't cached.
    const good = vi.fn(async () => new Response(JSON.stringify(discovery), { status: 200 })) as unknown as typeof fetch;
    await expect(discover("https://idp.example.com", good)).resolves.toMatchObject({
      token_endpoint: "https://idp.example.com/token",
    });
  });
});

describe("pkce + random", () => {
  it("derives a stable S256 challenge for a verifier", async () => {
    const c1 = await pkceChallenge("verifier");
    const c2 = await pkceChallenge("verifier");
    expect(c1).toBe(c2);
    expect(c1).not.toContain("="); // base64url, unpadded
    expect(c1).not.toContain("+");
  });
  it("returns distinct random values", () => {
    expect(randomUrlSafe()).not.toBe(randomUrlSafe());
  });
});

describe("buildRedirectUri", () => {
  const config: OidcConfig = {
    issuer: "https://idp.example.com",
    clientId: "c",
    clientSecret: "s",
    scopes: "openid",
    allowedDomains: [],
    buttonLabel: "SSO",
  };
  it("derives the callback from the forwarded host", () => {
    const req = new NextRequest("https://chat.example.com/api/auth/oidc/start", {
      headers: { "x-forwarded-host": "chat.example.com", "x-forwarded-proto": "https" },
    });
    expect(buildRedirectUri(config, req)).toBe("https://chat.example.com/api/auth/oidc/callback");
  });
  it("honors a pinned redirect uri", () => {
    const req = new NextRequest("https://chat.example.com/api/auth/oidc/start");
    expect(buildRedirectUri({ ...config, redirectUri: "https://pinned.example.com/cb" }, req)).toBe(
      "https://pinned.example.com/cb",
    );
  });
});
