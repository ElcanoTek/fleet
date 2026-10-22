import { getBootstrap } from "@/app/lib/bootstrap";
import { NotFound } from "@/app/shared/ui/NotFound";
import { SharedConversationView } from "./SharedConversationView";

// Public read-only conversation share (#226), reachable without a session.
// The server fetches the snapshot per request (so revocation and expiry apply
// immediately) and renders it into the bootstrap; an unknown, revoked or
// expired token is a 404 with no snapshot.
export default function SharedConversationPage() {
  const { shared } = getBootstrap();
  if (!shared) return <NotFound />;
  return <SharedConversationView snapshot={shared} />;
}
