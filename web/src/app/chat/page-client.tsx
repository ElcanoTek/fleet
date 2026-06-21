"use client";

import dynamic from "next/dynamic";

const ChatExperience = dynamic(
  () => import("./ui/chat-experience").then((module) => module.ChatExperience),
  { ssr: false },
);

export function PageClient() {
  return <ChatExperience />;
}
