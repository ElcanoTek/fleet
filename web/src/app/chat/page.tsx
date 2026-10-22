import { getBootstrap } from "@/app/lib/bootstrap";
import { PageClient } from "./page-client";

// /chat. The membership entry gate for identities minted elsewhere (#1522)
// runs on the server before this page is served (internal/webui/spa.go): an
// elcano_auth or central-Auth session whose email is not on the user list is
// sent to /no-access. The already-resolved session email rides the bootstrap,
// so cold boot skips its serial /api/session round trip
// (ChatExperience.initialUserEmail).
export default function ChatPage() {
  return <PageClient initialEmail={getBootstrap().session?.email ?? null} />;
}
