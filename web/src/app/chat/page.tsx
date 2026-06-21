import { redirect } from "next/navigation";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { PageClient } from "./page-client";

// Membership entry gate for the Elcano-email path. Password sessions are
// members by construction (they must exist in the user-list to have a
// password), so we only pay a check for elcano_auth sessions. chat-server's
// membershipMiddleware is the real boundary — every API call is gated there —
// but this turns a validly-signed-in non-member's experience from a shell
// that 403s on every request into a single clear no-access page.
export default async function Home() {
  const session = await getServerSession();
  if (session?.source === "elcano") {
    let denied = false;
    try {
      const res = await chatServerFetch(session.email, "/auth/membership");
      denied = res.status === 403;
    } catch {
      // chat-server unreachable — don't trap the user on no-access for a
      // transient error; let the app load and surface failures normally.
      denied = false;
    }
    if (denied) {
      redirect("/no-access");
    }
  }
  return <PageClient />;
}
