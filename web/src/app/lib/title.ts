type MessageLike = {
  role: "assistant" | "user";
  content: string;
};

export function deriveConversationTitle(messages: MessageLike[]) {
  const firstUserMessage = messages.find((message) => message.role === "user");
  if (!firstUserMessage) {
    return "New chat";
  }

  return firstUserMessage.content.length > 44
    ? `${firstUserMessage.content.slice(0, 44).trim()}...`
    : firstUserMessage.content;
}
