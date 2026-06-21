import { cookies } from "next/headers";
import { NextRequest, NextResponse } from "next/server";
import {
  createSessionToken,
  getRedirectUrl,
  getSessionCookieName,
  sessionMaxAgeSeconds,
  isSecureRequest,
} from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

/**
 * POST /api/auth/login
 *
 * Auth model:
 *   1. Receive email + password from the login form.
 *   2. Forward to chat-server POST /auth/verify (shared-secret protected).
 *   3. chat-server looks up the user in its SQLite users table and
 *      bcrypt-compares the password.
 *   4. On success, mint the HMAC session cookie and redirect home.
 *
 * Users are provisioned via `chat user add <email>` on the server —
 * there is NO self-signup path and no shared password.
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const formData = await request.formData();
  const email = String(formData.get("email") ?? "")
    .trim()
    .toLowerCase();
  const password = String(formData.get("password") ?? "");

  if (!email || !password) {
    return redirectBackWithError(request, "missing");
  }

  // chatServerFetch requires a userEmail for the X-User-Email header. For
  // the pre-login verify call we just echo the attempted email — the
  // server-side verify path doesn't consult that header for authz.
  let upstream: Response;
  try {
    upstream = await chatServerFetch(email, "/auth/verify", {
      method: "POST",
      body: JSON.stringify({ email, password }),
    });
  } catch (err) {
    console.error("chat-server unreachable:", err);
    return redirectBackWithError(request, "server");
  }

  if (!upstream.ok) {
    return redirectBackWithError(request, "server");
  }

  const verify = (await upstream.json()) as { ok: boolean; error?: string };
  if (!verify.ok) {
    // Generic error class — don't reveal whether the email exists.
    return redirectBackWithError(request, "invalid");
  }

  const cookieStore = await cookies();
  cookieStore.set({
    name: getSessionCookieName(),
    value: await createSessionToken(email),
    httpOnly: true,
    sameSite: "lax",
    secure: isSecureRequest(request),
    maxAge: sessionMaxAgeSeconds,
    path: "/",
  });
  return NextResponse.redirect(getRedirectUrl(request, "/"), { status: 303 });
}

function redirectBackWithError(request: NextRequest, code: string) {
  const url = getRedirectUrl(request, `/login?e=${code}`);
  return NextResponse.redirect(url, { status: 303 });
}
