import { NextRequest, NextResponse } from "next/server";
import { verifyBackchannelLogoutToken } from "@/app/lib/backchannelLogout";
import { revokeExternalSessions } from "@/app/lib/chatServer";
import { getOidcConfig } from "@/app/lib/oidc";

export const runtime = "nodejs";
const maxBackchannelBodyBytes = 20_000;

export async function POST(request: NextRequest): Promise<NextResponse> {
  const config = getOidcConfig();
  if (!config) return new NextResponse(null, { status: 404 });
  const contentLengthHeader = request.headers.get("content-length");
  if (contentLengthHeader === null) return new NextResponse(null, { status: 411 });
  if (!/^\d+$/.test(contentLengthHeader)) return new NextResponse(null, { status: 400 });
  if (Number(contentLengthHeader) > maxBackchannelBodyBytes) {
    return new NextResponse(null, { status: 413 });
  }
  let raw: FormDataEntryValue | null;
  try {
    raw = (await request.formData()).get("logout_token");
  } catch {
    return new NextResponse(null, { status: 400 });
  }
  if (typeof raw !== "string") return new NextResponse(null, { status: 400 });
  let event;
  try {
    event = await verifyBackchannelLogoutToken(raw, config.issuer, config.clientId);
  } catch {
    return new NextResponse(null, { status: 400 });
  }
  const revoked = await revokeExternalSessions(
    event.email,
    event.eventId,
    event.issuer,
    event.subject,
  );
  return new NextResponse(null, { status: revoked ? 204 : 503 });
}
