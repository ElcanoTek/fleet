import { afterEach, describe, expect, it } from "vitest";
import { NextRequest } from "next/server";
import { GET } from "./route";

// The "Use Elcano email" button target: a 303 redirect to the auth service's
// magic-link login, signed back to chat's home page via return_to.

describe("GET /api/auth/elcano-login", () => {
  afterEach(() => {
    delete process.env.AUTH_LOGIN_URL;
    delete process.env.AUTH_SIGNING_PUBKEY;
  });

  it("redirects to the auth login URL with return_to = chat home", async () => {
    process.env.AUTH_LOGIN_URL = "https://auth.elcanotek.com";
    process.env.AUTH_SIGNING_PUBKEY = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMzI="; // any non-empty value
    const req = new NextRequest("https://chat.elcanotek.com/api/auth/elcano-login");

    const res = await GET(req);

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(
      "https://auth.elcanotek.com/?return_to=https%3A%2F%2Fchat.elcanotek.com%2F",
    );
  });

  it("honors a dev AUTH_LOGIN_URL and the forwarded host", async () => {
    process.env.AUTH_LOGIN_URL = "http://localhost:9000";
    process.env.AUTH_SIGNING_PUBKEY = "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMzI=";
    const req = new NextRequest("http://localhost:3000/api/auth/elcano-login", {
      headers: { "x-forwarded-host": "localhost:3000", "x-forwarded-proto": "http" },
    });

    const res = await GET(req);

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(
      "http://localhost:9000/?return_to=http%3A%2F%2Flocalhost%3A3000%2F",
    );
  });

  it("bounces to /login with an error when no public key is configured (avoids redirect loop)", async () => {
    delete process.env.AUTH_SIGNING_PUBKEY;
    const req = new NextRequest("https://chat.elcanotek.com/api/auth/elcano-login");

    const res = await GET(req);

    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe("https://chat.elcanotek.com/login?e=elcano_unavailable");
  });
});
