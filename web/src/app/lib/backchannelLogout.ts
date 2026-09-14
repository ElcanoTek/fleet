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

function signingPublicKeys(): string[] {
  return [
    process.env.AUTH_SIGNING_PUBKEY ?? "",
    ...(process.env.AUTH_SIGNING_PREVIOUS_PUBKEYS ?? "").split(","),
  ]
    .map((value) => value.trim())
    .filter(Boolean);
}

export async function verifyBackchannelLogoutToken(
  raw: string,
  issuer: string,
  audience: string,
  nowSeconds: number = Math.floor(Date.now() / 1000),
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
  for (const encoded of signingPublicKeys()) {
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
