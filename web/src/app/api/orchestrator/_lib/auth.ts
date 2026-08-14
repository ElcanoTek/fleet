import type { NextRequest } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import type { OrchestratorAuth } from "@/app/lib/orchestratorServer";

// resolveOrchestratorAuth picks the credential the orchestrator proxy routes
// forward upstream. BOTH login paths are honored:
//   1. A moc username/password Bearer token on the incoming request wins (it's
//      the explicit moc session) and is forwarded verbatim.
//   2. Otherwise an elcano session cookie (Ed25519 elcano_auth OR HMAC
//      elcano_session) supplies the user email AND its session epoch, which the
//      orchestrator checks against the chat account so a password reset ends
//      this plane's access too. Forwarding the whole session rather than
//      `session.email` is deliberate: the epoch is half of the identity.
// Returns null when neither is present, so the route can 401.
export async function resolveOrchestratorAuth(
  request: NextRequest,
): Promise<OrchestratorAuth | null> {
  const authHeader = request.headers.get("authorization");
  if (authHeader && /^Bearer\s+\S/i.test(authHeader)) {
    const token = authHeader.replace(/^Bearer\s+/i, "").trim();
    if (token) return { kind: "bearer", token };
  }

  const session = await getServerSession();
  if (session) return { kind: "cookie", email: session.email, epoch: session.epoch };

  return null;
}
