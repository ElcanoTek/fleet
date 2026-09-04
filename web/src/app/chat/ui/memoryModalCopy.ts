// The memories modal's subtitle, per tab.
//
// One string used to describe all three tabs: "Saved memories are scoped to
// {email} and are added to future chats." On the Team learnings tab neither
// half of that is true — team learnings are scoped to the project and its
// team, not to one account, and they are injected only into that project's
// chats, not into every future chat (finding #18). On the Graph tab it
// described records rather than the derived entities and relations the tab
// actually shows.
//
// Extracted as a pure function so each tab's claim is asserted directly
// against what that tab holds.

export type MemoryModalTab = "list" | "graph" | "team";

export function memoryModalSubtitle(
  tab: MemoryModalTab,
  {
    userEmail,
    projectName,
  }: {
    userEmail?: string;
    // The open chat's project. The "team" tab only exists inside one.
    projectName?: string;
  },
): string {
  if (tab === "team" && projectName) {
    return `Team learnings are shared with everyone in ${projectName} and added to every chat in it.`;
  }
  if (tab === "graph") {
    return "Entities and relations derived from your own saved memories, each linked to the memory it came from.";
  }
  return `Saved memories are scoped to ${userEmail || "this user"} and are added to future chats.`;
}
