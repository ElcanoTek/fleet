import { getBootstrap } from "@/app/lib/bootstrap";
import { OrchestratorClient } from "./orchestrator-client";

// View B — the Operations Center. Whether to surface the "Use Elcano email"
// button (AUTH_SIGNING_PUBKEY is configured) arrives in the bootstrap.
export default function OrchestratorPage() {
  return <OrchestratorClient magicLinkLoginEnabled={getBootstrap().login.magicLinkLoginEnabled} />;
}
