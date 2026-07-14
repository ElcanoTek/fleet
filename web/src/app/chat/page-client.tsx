"use client";

import { ChatExperience } from "./ui/chat-experience";

export function PageClient({ initialEmail }: { initialEmail?: string | null }) {
  return <ChatExperience initialUserEmail={initialEmail ?? null} />;
}
