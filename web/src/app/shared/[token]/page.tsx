import { notFound } from "next/navigation";
import { chatServerFetchPublic } from "@/app/lib/chatServer";
import { SharedConversationView, type SharedSnapshot } from "./SharedConversationView";

// A shared snapshot must reflect revocation/expiry immediately, so never cache
// or statically pre-render it — always fetch per request.
export const dynamic = "force-dynamic";

// Public read-only conversation share (#226). Reachable without a session
// (middleware bypasses /shared/*). Fetches the snapshot from chat-server with
// the shared secret but NO user identity (chatServerFetchPublic); an unknown,
// revoked, or expired token comes back non-2xx → notFound().
export default async function SharedConversationPage({
  params,
}: {
  params: Promise<{ token: string }>;
}) {
  const { token } = await params;
  let res: Response;
  try {
    res = await chatServerFetchPublic(`/shared/${encodeURIComponent(token)}`);
  } catch {
    notFound();
  }
  if (!res.ok) {
    notFound();
  }
  const snapshot = (await res.json()) as SharedSnapshot;
  return <SharedConversationView snapshot={snapshot} />;
}
