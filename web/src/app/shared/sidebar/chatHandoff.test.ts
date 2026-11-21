import { afterEach, describe, expect, it, vi } from "vitest";

const navigate = vi.fn();
vi.mock("@/app/lib/navigation", () => ({ navigate: (href: string) => navigate(href) }));

import { chatHref, handOffToChat, handoffHref, hasChatHandoff, projectHref, takeChatHandoff, teamChatHref } from "./chatHandoff";

const chat = { id: "c 1", title: "Board" } as never;

afterEach(() => {
  takeChatHandoff();
  navigate.mockClear();
});

describe("handing an action over to Chat", () => {
  it("notes the action, goes to Chat, and Chat takes it once", () => {
    handOffToChat({ kind: "chat", action: "delete", conversation: chat });
    expect(navigate).toHaveBeenCalledWith("/chat");
    expect(hasChatHandoff()).toBe(true);
    expect(takeChatHandoff()).toEqual({ kind: "chat", action: "delete", conversation: chat });
    // Taken: a later visit to Chat must not delete anything.
    expect(takeChatHandoff()).toBeNull();
    expect(hasChatHandoff()).toBe(false);
  });

  it("opens the chat first for the dialogs that are about the chat on screen", () => {
    for (const action of ["share", "download", "transfer", "workflow"] as const) {
      expect(handoffHref({ kind: "chat", action, conversation: chat })).toBe("/chat?c=c%201");
    }
    // A patrol or a task opens its chat itself; delete and archive need none open.
    for (const action of ["patrol", "task", "delete", "archive", "unarchive"] as const) {
      expect(handoffHref({ kind: "chat", action, conversation: chat })).toBe("/chat");
    }
  });

  it("a new chat starts empty instead of opening the most recent one", () => {
    expect(handoffHref({ kind: "new-chat" })).toBe("/chat?draft=");
    expect(handoffHref({ kind: "new-chat", sealed: true })).toBe("/chat?draft=");
    expect(handoffHref({ kind: "new-project" })).toBe("/chat");
    expect(handoffHref({ kind: "select" })).toBe("/chat");
  });

  it("the latest action wins: a second click replaces the first", () => {
    handOffToChat({ kind: "new-project" });
    handOffToChat({ kind: "delete-temporary" });
    expect(takeChatHandoff()).toEqual({ kind: "delete-temporary" });
  });
});

describe("where a row opens from a page that is not Chat", () => {
  it("links a chat, a project, and a teammate's chat by how it is shared", () => {
    expect(chatHref({ id: "a/b" })).toBe("/chat?c=a%2Fb");
    expect(projectHref("p 1")).toBe("/chat?project=p%201");
    // Shared with the team: the team link decides between the viewer and the chat.
    expect(teamChatHref({ id: "t1", team_visible: true })).toBe("/chat?team=t1");
    // Multiplayer, or reached only as a participant: the chat itself.
    expect(teamChatHref({ id: "t1", team_visible: true, multiplayer: true })).toBe("/chat?c=t1");
    expect(teamChatHref({ id: "t1" })).toBe("/chat?c=t1");
  });
});
