"use client";

import { ChatToastProvider } from "./ui/ChatToasts";
import { ChatExperience } from "./ui/chat-experience";

export function PageClient({ initialEmail }: { initialEmail?: string | null }) {
  return (
    <ChatToastProvider>
      <ChatExperience initialUserEmail={initialEmail ?? null} />
    </ChatToastProvider>
  );
}
