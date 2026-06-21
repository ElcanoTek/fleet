import { getAuthSigningPubkey } from "@/app/lib/auth";
import { OrchestratorClient } from "./orchestrator-client";

// View B — the orchestrator dashboard (was moc, re-ported to React). Server
// component so it can read AUTH_SIGNING_PUBKEY at request time (the same gate
// chat's /login uses) to decide whether to surface the "Use Elcano email"
// button. force-dynamic for the same reason chat's login is dynamic: the
// deploy build runs without .env.local, so the flag must resolve at runtime.
export const dynamic = "force-dynamic";

export default function OrchestratorPage() {
  return <OrchestratorClient elcanoLoginEnabled={getAuthSigningPubkey() !== ""} />;
}
