import { redirect } from "next/navigation";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { PageClient } from "./page-client";

// Membership entry gate for identities minted elsewhere. Password sessions
// are members by construction (they must exist in the user-list to have a
// password). Sessions from the shared elcano_auth cookie and from central
// Auth (OIDC) are not: Auth decides who may reach Fleet, Fleet's user-list
// decides who is a member, and an account that passes the first but not the
// second must see one clear no-access page — not a shell whose every API call
// 403s and whose 401 handlers bounce it between /login and /chat (#1522).
// chat-server's membershipMiddleware remains the real boundary.
export default async function Home() {
  const session = await getServerSession();
  if (session?.source === "elcano" || session?.source === "oidc") {
    let denied = false;
    try {
      const res = await chatServerFetch(session, "/auth/membership");
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
  // Hand the already-resolved session email to the client so cold boot skips
  // its serial /api/session round-trip (see ChatExperience.initialUserEmail).
  return <PageClient initialEmail={session?.email ?? null} />;
}
