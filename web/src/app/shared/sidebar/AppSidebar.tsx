import { t as tr } from "@/app/lib/i18n";
import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { ConversationSidebar } from "@/app/chat/ui/ConversationSidebar";
import type { ConversationSummary } from "@/app/chat/ui/chat-experience";
import { navigate } from "@/app/lib/navigation";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { useSessionEmail } from "@/app/shared/hooks/useSessionEmail";
import { signOut } from "@/app/shared/signOut";
import type { RailCollapse, RailView } from "@/app/shared/ui/NavRail";
import { chatHref, handOffToChat, projectHref, teamChatHref, type ChatRowAction } from "./chatHandoff";
import { useSidebarLibrary } from "./useSidebarLibrary";

// AppSidebar — the sidebar on every page that is not Chat (docs/SIDEBAR.md):
// Background tasks, Patrols, Settings, Guides, Analytics. It is the same
// component Chat renders (ConversationSidebar), fed from useSidebarLibrary
// instead of Chat's own state, so wherever a person is they see their pinned
// chats, their projects and their chats, and one click takes them back to
// any of them.
//
// What differs from Chat is only where things happen. Opening a chat or a
// project goes to Chat. A change that is one request (pin, rename, labels,
// reorder) is made here. Anything that needs one of Chat's dialogs is handed
// to Chat, which opens with that dialog up (chatHandoff.ts).

const isMac = () => typeof navigator !== "undefined" && /Mac|iPhone|iPad/i.test(navigator.platform || navigator.userAgent);

export function AppSidebar({
  activeView,
  opsCount,
  sidebarOpen,
  setSidebarOpen,
  collapse,
  onSignOut = signOut,
  onNewTask,
}: {
  activeView: RailView;
  opsCount?: number;
  sidebarOpen: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
  collapse: RailCollapse;
  onSignOut?: () => void;
  // Background tasks opens its own new-task form; elsewhere (undefined) the
  // + menu goes to that page with the form open. null: not offered here.
  onNewTask?: (() => void) | null;
}) {
  const { branding } = useClientConfig();
  const userEmail = useSessionEmail();
  // A change that did not save: said in the sidebar, where it was made.
  const [error, setError] = useState<string | null>(null);
  const errorTimer = useRef<number | null>(null);
  const showError = useCallback((message: string) => {
    setError(message);
    if (errorTimer.current !== null) window.clearTimeout(errorTimer.current);
    errorTimer.current = window.setTimeout(() => setError(null), 6000);
  }, []);
  useEffect(
    () => () => {
      if (errorTimer.current !== null) window.clearTimeout(errorTimer.current);
    },
    [],
  );
  const lib = useSidebarLibrary(userEmail, showError);
  const searchRef = useRef<HTMLInputElement | null>(null);
  const [showArchived, setShowArchived] = useState(false);
  const searchShortcut = isMac() ? "⌘K" : "Ctrl+K";

  // ⌘K / Ctrl+K puts the cursor in the search field, as it does in Chat.
  const { collapsed, setCollapsed } = collapse;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== "k" || !(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey) return;
      e.preventDefault();
      if (collapsed) setCollapsed(false);
      setSidebarOpen(true);
      requestAnimationFrame(() => searchRef.current?.focus());
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [collapsed, setCollapsed, setSidebarOpen]);

  const inChat = (action: ChatRowAction) => (conversation: ConversationSummary) =>
    handOffToChat({ kind: "chat", action, conversation });

  return (
    <ConversationSidebar
      activeView={activeView}
      opsCount={opsCount}
      sidebarOpen={sidebarOpen}
      setSidebarOpen={setSidebarOpen}
      collapse={collapse}
      branding={branding}
      serverConfig={lib.serverConfig}
      userEmail={userEmail}
      onSignOut={onSignOut}
      clearConversation={(opts) => handOffToChat({ kind: "new-chat", sealed: opts?.lockdown })}
      sidebarQuery={lib.query}
      setSidebarQuery={lib.setQuery}
      searchRef={searchRef}
      filterLabels={lib.filterLabels}
      setFilterLabels={lib.setFilterLabels}
      isLoadingHistory={lib.loading}
      conversations={lib.conversations}
      filteredConversations={lib.filteredConversations}
      activeConversationId={null}
      focusedConversationId={null}
      renameSignal={null}
      loadConversation={async (id) => navigate(chatHref({ id }))}
      togglePin={lib.togglePin}
      chatOrders={lib.chatOrders}
      onReorderChats={lib.canWrite ? lib.reorderChats : undefined}
      canPinTeamChats={lib.canWrite}
      readOnly={lib.readOnly}
      recentSort={lib.recentSort}
      onRecentSortChange={lib.setRecentSort}
      toggleArchive={async (c, archived) => inChat(archived ? "archive" : "unarchive")(c)}
      renameConversation={lib.renameConversation}
      downloadConversation={async (c) => inChat("download")(c)}
      promoteConversation={async (c) => inChat("task")(c)}
      savePromptFromConversation={inChat("workflow")}
      onOpenPatrols={inChat("patrol")}
      setPendingDeleteConversation={(next) => {
        const pending = typeof next === "function" ? next(null) : next;
        const conversation =
          pending &&
          (lib.conversations.find((c) => c.id === pending.id) ??
            lib.archivedConversations.find((c) => c.id === pending.id));
        if (conversation) inChat("delete")(conversation);
      }}
      setConversationLabels={lib.setConversationLabels}
      openShareDialog={inChat("share")}
      openTransferDialog={inChat("transfer")}
      canTransferTeamChats={lib.admin}
      archivedConversations={lib.archivedConversations}
      showArchived={showArchived}
      setShowArchived={setShowArchived}
      setConfirmBulkDelete={() => handOffToChat({ kind: "delete-temporary" })}
      // Selecting is a mode of Chat's sidebar, with Chat's bulk actions.
      onEnterSelectMode={() => handOffToChat({ kind: "select" })}
      searchShortcut={searchShortcut}
      // The new-chat chord is Chat's.
      newChatShortcut={false}
      onCreateProject={() => handOffToChat({ kind: "new-project" })}
      onNewTask={onNewTask}
      onOpenProjectHome={(projectID, settings) => navigate(projectHref(projectID) + (settings ? "&settings=1" : ""))}
      onPinProject={lib.pinProject}
      onNewChatInProject={(projectId) => handOffToChat({ kind: "project", action: "new-chat", projectId })}
      onRenameProject={lib.renameProject}
      onDeleteProject={(projectId) => handOffToChat({ kind: "project", action: "delete", projectId })}
      projects={lib.projects}
      teamSharedConversations={lib.teamSharedConversations}
      onSelectTeamSharedConversation={(c) => navigate(teamChatHref(c))}
      onMoveToProject={(conversationId, projectId) => handOffToChat({ kind: "move", conversationId, projectId })}
      projectsLoaded={lib.projectsLoaded}
      teamSharedLoaded={lib.teamSharedLoaded}
      archivedLoaded={lib.archivedLoaded}
      notice={
        error ? (
          <p role="alert" data-testid="sidebar-error" className="m-0 mb-1.5 rounded-[var(--radius-md)] border border-[var(--color-status-error-border)] bg-[var(--color-status-error-bg)] px-2.5 py-1.5 text-[0.78rem] leading-snug text-[var(--color-status-error-fg)]">
            {error}{" "}
            <button type="button" className="underline underline-offset-2" onClick={() => setError(null)}>
              {tr("Dismiss")}
            </button>
          </p>
        ) : null
      }
    />
  );
}
