import { NextRequest, NextResponse } from "next/server";
import { buildElcanoLoginUrl, getAuthSigningPubkey, getRedirectUrl } from "@/app/lib/auth";

export const runtime = "nodejs";

// GET /api/orchestrator/auth/elcano-login
//
// Target of the "Use Elcano email" button on the orchestrator login card.
// Identical handoff to chat's elcano-login, but signs the browser back to the
// /orchestrator view instead of /chat so the user lands where they started.
export async function GET(request: NextRequest) {
  if (!getAuthSigningPubkey()) {
    return NextResponse.redirect(
      getRedirectUrl(request, "/login?e=elcano_unavailable"),
      { status: 303 },
    );
  }
  const returnTo = getRedirectUrl(request, "/orchestrator").toString();
  return NextResponse.redirect(buildElcanoLoginUrl(returnTo), { status: 303 });
}
