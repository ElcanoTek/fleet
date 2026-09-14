const logoutEvent = "http://schemas.openid.net/event/backchannel-logout";
const encoder = new TextEncoder();

export type BackchannelLogout = {
  eventId: string;
  subject: string;
  email: string;
  issuer: string;
};

function base64UrlBytes(value: string): Uint8Array<ArrayBuffer> {
  if (!value || !/^[A-Za-z0-9_-]+$/.test(value)) throw new Error("invalid logout token");
  const decoded = Buffer.from(value, "base64url");
  const bytes = new Uint8Array(decoded.length);
  bytes.set(decoded);
  return bytes;
}

function staticSigningPublicKeys(): string[] {
  return [
    process.env.AUTH_SIGNING_PUBKEY ?? "",
    ...(process.env.AUTH_SIGNING_PREVIOUS_PUBKEYS ?? "").split(","),
  ]
    .map((value) => value.trim())
    .filter(Boolean);
}

// Auth publishes its current and rotation keys at <issuer>/jwks.json. Reading
// that makes a signing-key rotation a one-sided change on Auth instead of an
// env edit on every Fleet host. Static env keys stay as bootstrap and offline
// fallback: a fetch failure keeps whatever was cached, and a token whose key
// is already known never waits on the network. Refreshes are rate-limited so
// tokens with unknown kids cannot amplify requests to Auth.
const jwksCacheMs = 10 * 60 * 1000;
const jwksMinRefreshMs = 60 * 1000;
const maxJwksBytes = 64 * 1024;

type JwksCache = { keys: string[]; fetchedAt: number; lastAttempt: number };
const jwksCache = new Map<string, JwksCache>();

async function fetchJwksKeys(issuer: string, fetchImpl: typeof fetch): Promise<string[] | null> {
  try {
    const res = await fetchImpl(`${issuer.replace(/\/+$/, "")}/jwks.json`, {
      headers: { Accept: "application/json" },
      redirect: "error",
    });
    if (!res.ok) return null;
    const text = await res.text();
    if (text.length > maxJwksBytes) return null;
    const doc = JSON.parse(text) as { keys?: unknown };
    if (!Array.isArray(doc.keys)) return null;
    const keys: string[] = [];
    for (const entry of doc.keys) {
      if (!entry || typeof entry !== "object") continue;
      const jwk = entry as Record<string, unknown>;
      if (jwk.kty !== "OKP" || jwk.crv !== "Ed25519" || typeof jwk.x !== "string") continue;
      let raw: Buffer;
      try {
        raw = Buffer.from(jwk.x, "base64url");
      } catch {
        continue;
      }
      if (raw.length !== 32) continue;
      keys.push(raw.toString("base64"));
    }
    return keys;
  } catch {
    return null;
  }
}

async function refreshJwks(issuer: string, now: number, fetchImpl: typeof fetch, force = false): Promise<void> {
  const cached = jwksCache.get(issuer);
  if (!force && cached && now - cached.lastAttempt < jwksMinRefreshMs) return;
  const previous = cached ?? { keys: [], fetchedAt: 0, lastAttempt: 0 };
  jwksCache.set(issuer, { ...previous, lastAttempt: now });
  const keys = await fetchJwksKeys(issuer, fetchImpl);
  if (keys === null) return;
  jwksCache.set(issuer, { keys, fetchedAt: now, lastAttempt: now });
}

async function kidForKey(encoded: string): Promise<string | null> {
  const publicKey = Uint8Array.from(Buffer.from(encoded, "base64"));
  if (publicKey.length !== 32) return null;
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", publicKey));
  return Buffer.from(digest.slice(0, 16)).toString("base64url");
}

/**
 * resolveSigningKeys returns the keys to try for one token: static env keys
 * plus Auth's published JWKS (cached ten minutes), refreshing once when the
 * token names a kid none of them match.
 */
export async function resolveSigningKeys(
  issuer: string,
  kid: string | null,
  fetchImpl: typeof fetch = fetch,
  now: number = Date.now(),
): Promise<string[]> {
  const cached = jwksCache.get(issuer);
  if (!cached || now - cached.fetchedAt > jwksCacheMs) await refreshJwks(issuer, now, fetchImpl);
  const merge = () => {
    const seen = new Set<string>();
    const out: string[] = [];
    for (const key of [...staticSigningPublicKeys(), ...(jwksCache.get(issuer)?.keys ?? [])]) {
      if (!seen.has(key)) {
        seen.add(key);
        out.push(key);
      }
    }
    return out;
  };
  let keys = merge();
  if (kid) {
    let known = false;
    for (const key of keys) {
      if ((await kidForKey(key)) === kid) {
        known = true;
        break;
      }
    }
    if (!known) {
      await refreshJwks(issuer, now, fetchImpl);
      keys = merge();
    }
  }
  return keys;
}

/** Test seam: forget cached JWKS documents. */
export function resetJwksCacheForTests(): void {
  jwksCache.clear();
}

export async function verifyBackchannelLogoutToken(
  raw: string,
  issuer: string,
  audience: string,
  nowSeconds: number = Math.floor(Date.now() / 1000),
  fetchImpl: typeof fetch = fetch,
): Promise<BackchannelLogout> {
  if (raw.length > 16_384) throw new Error("invalid logout token");
  const parts = raw.split(".");
  if (parts.length !== 3) throw new Error("invalid logout token");
  let header: Record<string, unknown>;
  let claims: Record<string, unknown>;
  try {
    header = JSON.parse(new TextDecoder().decode(base64UrlBytes(parts[0]))) as Record<string, unknown>;
    claims = JSON.parse(new TextDecoder().decode(base64UrlBytes(parts[1]))) as Record<string, unknown>;
  } catch {
    throw new Error("invalid logout token");
  }
  if (header.typ !== "logout+jwt" || header.alg !== "EdDSA" || typeof header.kid !== "string") {
    throw new Error("invalid logout token");
  }

  let verified = false;
  const candidateKeys = await resolveSigningKeys(issuer, header.kid, fetchImpl, nowSeconds * 1000);
  for (const encoded of candidateKeys) {
    try {
      const publicKey = Uint8Array.from(Buffer.from(encoded, "base64"));
      if (publicKey.length !== 32) continue;
      const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", publicKey));
      const kid = Buffer.from(digest.slice(0, 16)).toString("base64url");
      if (kid !== header.kid) continue;
      const key = await crypto.subtle.importKey("raw", publicKey, { name: "Ed25519" }, false, [
        "verify",
      ]);
      verified = await crypto.subtle.verify(
        { name: "Ed25519" },
        key,
        base64UrlBytes(parts[2]),
        encoder.encode(`${parts[0]}.${parts[1]}`),
      );
      if (verified) break;
    } catch {
      continue;
    }
  }
  if (!verified) throw new Error("invalid logout token");

  const normalizedIssuer = issuer.replace(/\/+$/, "");
  const subject = typeof claims.sub === "string" ? claims.sub.trim() : "";
  const email = typeof claims.email === "string" ? claims.email.trim().toLowerCase() : "";
  const at = email.lastIndexOf("@");
  const eventId = typeof claims.jti === "string" ? claims.jti.trim() : "";
  const events = claims.events as Record<string, unknown> | undefined;
  if (
    (typeof claims.iss !== "string" ? "" : claims.iss.replace(/\/+$/, "")) !== normalizedIssuer ||
    claims.aud !== audience ||
    !subject ||
    subject.length > 255 ||
    !email ||
    email.length > 254 ||
    at <= 0 ||
    at === email.length - 1 ||
    !eventId ||
    eventId.length > 255 ||
    typeof claims.iat !== "number" ||
    !Number.isInteger(claims.iat) ||
    claims.iat <= 0 ||
    claims.iat > nowSeconds + 60 ||
    // Back-Channel Logout 1.0 requires exp; Auth signs a fresh iat/exp per
    // delivery attempt, so a retry hours after the revocation still passes.
    typeof claims.exp !== "number" ||
    !Number.isInteger(claims.exp) ||
    claims.exp + 60 <= nowSeconds ||
    !events ||
    typeof events[logoutEvent] !== "object" ||
    events[logoutEvent] === null ||
    "nonce" in claims
  ) {
    throw new Error("invalid logout token");
  }
  return { eventId, subject, email, issuer: normalizedIssuer };
}
