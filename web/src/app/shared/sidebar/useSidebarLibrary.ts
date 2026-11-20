import { t as tr } from "@/app/lib/i18n";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ConversationSummary, ServerConfig } from "@/app/chat/ui/chat-experience";
import type { Project } from "@/app/chat/ui/ProjectsModal";
import { readChatSession } from "@/app/chat/ui/chatSessionStore";
import {
  RECENT_SORT_ITEM,
  filterConversations,
  isRecentSort,
  type RecentSort,
} from "@/app/chat/ui/conversationOrganization";
import { conversationApiUrl } from "@/app/lib/conversationApiUrl";
import { fetchMe } from "@/app/shared/lib/account";
import { readPref, writePref } from "@/app/shared/lib/storage";

// useSidebarLibrary — what the sidebar lists, for a page that is not Chat
// (docs/SIDEBAR.md). Chat holds its chats, teammates' chats and projects in
// its own state and passes them to the sidebar; Background tasks, Patrols,
// Settings, Guides and Analytics have none of that, so they read the same
// lists here: once per page, from the same endpoints, starting from whatever
// the last page left (so moving between pages does not flash an empty
// sidebar).
//
// It also carries the handful of changes that are one request and need no
// dialog: pin, rename, labels, the hand-set order, a project's pin and name.
// Everything else a row's menu offers is handed to Chat (chatHandoff.ts).

export type SidebarLibrary = {
  conversations: ConversationSummary[];
  archivedConversations: ConversationSummary[];
  teamSharedConversations: ConversationSummary[];
  projects: Project[];
  chatOrders: Record<string, string[]>;
  serverConfig: ServerConfig;
};

const DEFAULT_SERVER_CONFIG: ServerConfig = {
  lockdownAvailable: false,
  lockdownOnly: false,
  lockdownAllowedModels: [],
  uploadMaxBytes: 0,
};

// What the last page's sidebar showed, kept for the next page's first paint.
// Chat writes it too (rememberSidebarLibrary), so leaving Chat for another
// page shows the same lists at once.
let remembered: Partial<SidebarLibrary> | null = null;

export function rememberSidebarLibrary(next: Partial<SidebarLibrary>) {
  remembered = { ...remembered, ...next };
}

/** Test hook. */
export function resetSidebarLibraryForTests() {
  remembered = null;
}

function seed(): { library: SidebarLibrary; warm: boolean } {
  const chat = readChatSession();
  const library: SidebarLibrary = {
    conversations: remembered?.conversations ?? chat?.conversations ?? [],
    archivedConversations: remembered?.archivedConversations ?? chat?.archivedConversations ?? [],
    teamSharedConversations: remembered?.teamSharedConversations ?? [],
    projects: remembered?.projects ?? [],
    chatOrders: remembered?.chatOrders ?? {},
    serverConfig: remembered?.serverConfig ?? chat?.serverConfig ?? DEFAULT_SERVER_CONFIG,
  };
  return { library, warm: Boolean(remembered?.conversations ?? chat?.conversations) };
}

async function readJSON<T>(url: string): Promise<T | null> {
  try {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  }
}

type ServerConfigPayload = {
  lockdown_available?: boolean;
  lockdown_only?: boolean;
  lockdown_allowed_models?: string[] | null;
  conversation_ttl_days?: number;
  conversation_expiry_warning_days?: number;
  conversation_unpinned_cap?: number;
};

// The part of GET /api/server-config the sidebar reads: whether sealed chats
// exist here, and the retention numbers its wording quotes.
function sidebarServerConfig(cfg: ServerConfigPayload): ServerConfig {
  const positive = (n: unknown) => (typeof n === "number" && n > 0 ? n : undefined);
  return {
    ...DEFAULT_SERVER_CONFIG,
    lockdownAvailable: cfg.lockdown_available === true,
    lockdownOnly: cfg.lockdown_only === true,
    lockdownAllowedModels: cfg.lockdown_allowed_models ?? [],
    conversationTtlDays: positive(cfg.conversation_ttl_days),
    conversationExpiryWarningDays:
      typeof cfg.conversation_expiry_warning_days === "number" && cfg.conversation_expiry_warning_days >= 0
        ? cfg.conversation_expiry_warning_days
        : undefined,
    conversationUnpinnedCap: positive(cfg.conversation_unpinned_cap),
  };
}

export function useSidebarLibrary(userEmail: string, onError: (message: string) => void) {
  const [initial] = useState(seed);
  const [library, setLibraryState] = useState<SidebarLibrary>(initial.library);
  const [loading, setLoading] = useState(!initial.warm);
  const [projectsLoaded, setProjectsLoaded] = useState(Boolean(remembered?.projects));
  const [teamSharedLoaded, setTeamSharedLoaded] = useState(Boolean(remembered?.teamSharedConversations));
  const [archivedLoaded, setArchivedLoaded] = useState(false);
  const [role, setRole] = useState<string | undefined>(undefined);
  const [admin, setAdmin] = useState(false);
  // Bumped by every local change: a list read that started before it carries
  // the old state and is dropped rather than undoing the change on screen.
  const gen = useRef(0);

  const setLibrary = useCallback((update: (cur: SidebarLibrary) => SidebarLibrary) => {
    setLibraryState((cur) => {
      const next = update(cur);
      rememberSidebarLibrary(next);
      return next;
    });
  }, []);

  const mine = userEmail.trim().toLowerCase();
  const load = useCallback(async () => {
    const at = gen.current;
    const [own, archived, shared, projects, orders, config] = await Promise.all([
      readJSON<{ conversations?: ConversationSummary[] }>("/api/conversations"),
      readJSON<{ conversations?: ConversationSummary[] }>("/api/conversations?archived=true"),
      readJSON<{ conversations?: ConversationSummary[] }>("/api/conversations?scope=shared"),
      readJSON<{ projects?: Project[] }>("/api/projects"),
      readJSON<{ orders?: Record<string, string[]> }>("/api/conversations/order"),
      readJSON<ServerConfigPayload>("/api/server-config"),
    ]);
    setLoading(false);
    if (projects) setProjectsLoaded(true);
    if (shared) setTeamSharedLoaded(true);
    if (archived) setArchivedLoaded(true);
    // A change made while the lists were on their way wins: read again.
    if (at !== gen.current) return;
    setLibrary((cur) => ({
      // A list that could not be read keeps what was there.
      conversations: own ? (own.conversations ?? []) : cur.conversations,
      archivedConversations: archived ? (archived.conversations ?? []) : cur.archivedConversations,
      teamSharedConversations: shared
        ? (shared.conversations ?? []).filter((c) => !mine || (c.user_email ?? "").trim().toLowerCase() !== mine)
        : cur.teamSharedConversations,
      projects: projects ? (projects.projects ?? []) : cur.projects,
      chatOrders: orders?.orders && typeof orders.orders === "object" ? orders.orders : cur.chatOrders,
      serverConfig: config ? sidebarServerConfig(config) : cur.serverConfig,
    }));
  }, [mine, setLibrary]);

  useEffect(() => {
    let live = true;
    queueMicrotask(() => {
      if (live) void load();
    });
    void fetchMe().then((me) => {
      if (!live || !me) return;
      setRole(me.role || undefined);
      setAdmin(Boolean(me.admin));
    });
    return () => {
      live = false;
    };
  }, [load]);

  // One request, an optimistic change, and the lists read again if it failed.
  const change = useCallback(
    async (
      apply: (cur: SidebarLibrary) => SidebarLibrary,
      request: () => Promise<Response>,
      failed: string,
    ): Promise<boolean> => {
      gen.current++;
      setLibrary(apply);
      let ok = false;
      try {
        ok = (await request()).ok;
      } catch {
        ok = false;
      }
      gen.current++;
      if (!ok) {
        onError(failed);
        await load();
      }
      return ok;
    },
    [load, onError, setLibrary],
  );

  const post = (url: string, body: unknown, method = "POST") =>
    fetch(url, { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });

  const togglePin = useCallback(
    async (c: ConversationSummary): Promise<boolean> => {
      const url = conversationApiUrl(c.id, "/pin");
      if (!url) return false;
      const pinned = !c.pinned;
      const owner = (c.user_email ?? "").trim().toLowerCase();
      const theirs = Boolean(owner && mine && owner !== mine);
      const now = Math.floor(Date.now() / 1000);
      return change(
        (cur) =>
          theirs
            ? {
                ...cur,
                teamSharedConversations: cur.teamSharedConversations.map((x) => (x.id === c.id ? { ...x, pinned } : x)),
              }
            : {
                ...cur,
                // A pin or an unpin counts as activity on the server.
                conversations: cur.conversations
                  .map((x) => (x.id === c.id ? { ...x, pinned, updated_at: Math.max(x.updated_at, now) } : x))
                  .sort((a, b) => b.updated_at - a.updated_at),
              },
        () => post(url, { pinned }),
        pinned ? tr("Couldn’t pin “{0}”. Try again.", [c.title]) : tr("Couldn’t unpin “{0}”. Try again.", [c.title]),
      );
    },
    // post is a plain function of its arguments.
    [change, mine],
  );

  const renameConversation = useCallback(
    async (id: string, title: string): Promise<boolean> => {
      const trimmed = title.trim();
      const url = conversationApiUrl(id, "/rename");
      if (!trimmed || !url) return false;
      return change(
        (cur) => ({ ...cur, conversations: cur.conversations.map((c) => (c.id === id ? { ...c, title: trimmed } : c)) }),
        () => post(url, { title: trimmed }),
        tr("Couldn't rename the chat."),
      );
    },
    [change],
  );

  const setConversationLabels = useCallback(
    (id: string, labels: string[]) => {
      void change(
        (cur) => ({ ...cur, conversations: cur.conversations.map((c) => (c.id === id ? { ...c, labels } : c)) }),
        () => post("/api/conversations/bulk", { conversation_ids: [id], changes: { labels } }, "PATCH"),
        tr("Couldn't update {0} — network error.", [tr("the chat")]),
      );
    },
    [change],
  );

  const reorderChats = useCallback(
    (scope: string, ids: string[]) => {
      void change(
        (cur) => ({ ...cur, chatOrders: { ...cur.chatOrders, [scope]: ids } }),
        () => post("/api/conversations/order", { scope, conversation_ids: ids }, "PUT"),
        tr("Couldn’t save the new order. Try again."),
      );
    },
    [change],
  );

  const patchProject = useCallback(
    (id: string, patch: Partial<Pick<Project, "name" | "pinned">>, failed: string) => {
      void change(
        (cur) => ({ ...cur, projects: cur.projects.map((p) => (p.id === id ? { ...p, ...patch } : p)) }),
        () => post(`/api/projects/${encodeURIComponent(id)}`, patch, "PATCH"),
        failed,
      );
    },
    [change],
  );

  // The sort is a preference of this device, the one Chat reads.
  const [recentSort, setRecentSortState] = useState<RecentSort>(() => {
    const saved = readPref(RECENT_SORT_ITEM);
    return isRecentSort(saved) ? saved : "activity";
  });
  const setRecentSort = useCallback((next: RecentSort) => {
    setRecentSortState(next);
    writePref(RECENT_SORT_ITEM, next);
  }, []);

  const [query, setQuery] = useState("");
  const [filterLabels, setFilterLabels] = useState<string[]>([]);
  const filteredConversations = useMemo(
    () => filterConversations(library.conversations, { labels: filterLabels, query }),
    [library.conversations, filterLabels, query],
  );

  return {
    ...library,
    loading,
    projectsLoaded,
    teamSharedLoaded,
    archivedLoaded,
    // Reordering and pinning a teammate's chat are offered only once the
    // account is known not to be a read-only Viewer (the API refuses its
    // writes), the rule Chat follows.
    canWrite: role !== undefined && role !== "viewer",
    admin,
    togglePin,
    renameConversation,
    setConversationLabels,
    reorderChats,
    pinProject: (id: string, pinned: boolean) => patchProject(id, { pinned }, tr("Couldn't pin the project.")),
    renameProject: (id: string, name: string) => {
      const trimmed = name.trim();
      if (trimmed) patchProject(id, { name: trimmed }, tr("Couldn't rename the project."));
    },
    recentSort,
    setRecentSort,
    query,
    setQuery,
    filterLabels,
    setFilterLabels,
    filteredConversations,
    reload: load,
  };
}
