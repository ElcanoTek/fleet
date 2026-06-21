import { redirect } from "next/navigation";

// The unified frontend has two views. The bare root sends you to the chat
// view by default; the orchestrator lives at /orchestrator. Both are gated by
// the one middleware, so an authenticated user lands straight in /chat and an
// unauthenticated one is bounced to /login first.
export default function RootIndex() {
  redirect("/chat");
}
