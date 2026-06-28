import { NextRequest } from "next/server";
import { elcanoLoginRedirect } from "@/app/lib/auth";

export const runtime = "nodejs";

// GET /api/orchestrator/auth/elcano-login
//
// Target of the "Use Elcano email" button on the orchestrator login card. The
// SAME handoff as chat's elcano-login (shared elcanoLoginRedirect helper), but
// signs the browser back to the /orchestrator view instead of /chat so the user
// lands where they started.
export async function GET(request: NextRequest) {
  return elcanoLoginRedirect(request, "/orchestrator");
}
