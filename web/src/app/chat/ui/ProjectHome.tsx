"use client";

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import type { ConversationSummary } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { ConfirmDialog } from "./ConfirmDialog";
import { Icon } from "./Icon";
import { useChatToast } from "./ChatToasts";
import {
  fetchProjectMyState,
  formatDay,
  plural,
  shareChatWithTeam,
  sharedToast,
  updateProjectMyState,
  type ProjectMyState,
} from "./teamSharing";
import { ProjectVisibilityPill } from "./ProjectVisibilityPill";
import {
  buildGettingStarted,
  ProjectGettingStarted,
  type StepActionKind,
} from "./ProjectGettingStarted";
import { ProjectSources, type SourcesFocus } from "./ProjectSources";
import {
  TeamChatsSection,
  YourChatsSection,
  type ProjectChatEntry,
  type TeamChatEntry,
} from "./ProjectChatSections";

// Project home (#509 follow-up): the page a project's rail row opens — title,
// this member's chats in the project, the TEAM's shared chats beside them, a
// Sources panel (workspace files from those chats), and the two team-level
// context layers — Instructions and Team learnings — as a pair on the right.
// Per-project settings (name / sharing / transfer / delete) are
// ProjectSettingsDialog, which the parent renders when the gear or the
// visibility pill asks for it.
//
// The three context layers a project chat is fed by, named the way the UI now
// names them and in the order the prompt builder actually assembles them
// (internal/agent/prompt.go → buildSystemPrompt):
//
//   1. Instructions   — one field, owner-only, injected first.
//   2. Team learnings — the project's shared memory. Any member writes,
//                       approval-gated, every entry stamped with its author.
//   3. My memory      — the reader's own personal memories, everywhere.
//
// Layers 2 and 3 arrive in the same "User Memories" block, the project's
// entries tagged `[project]`. The helper copy under Instructions says exactly
// that, rather than naming two of the three.
//
// Privacy: "Your chats" shows the CALLER'S OWN conversations only — a
// team-shared project shares its definition, never a member's private chats.
// "Shared by your team" and Sources' "From your team" are the exceptions and
// are doubly gated: a shared team AND the owner's explicit per-chat opt-in
// (ADR-0013 / ADR-0057), plus, for files, the owner's per-file choice.
//
// Fleet Projects sharing (docs/TEAM-SHARING.md) made this page the place that
// teaches the team path: the header pill says who sees the project, the
// getting-started card walks a person to their first shared chat, every row
// carries its own Only you / <team> switch, and Sources groups files by chat.

// One team learning. user_email is the writer (provenance, recorded at write
// time); retired_at set = kept for the record but no longer injected.
type TeamLearning = {
  id: string;
  content: string;
  kind?: string;
  user_email?: string;
  pinned?: boolean;
  retired_at?: number | null;
  created_at?: number;
  updated_at?: number;
};

// The local part of an email — enough to say whose chat this is without
// turning every row into an address.
function shortName(email: string): string {
  const at = email.indexOf("@");
  return at > 0 ? email.slice(0, at) : email;
}

const cardClass =
  "rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4";

export function ProjectHome({
  project,
  chats,
  userEmail,
  isOwner,
  onBack,
  onOpenChat,
  onOpenTeamChat,
  onNewChat,
  onSaveInstructions,
  onUpdateSettings,
  myTeam,
  onOpenSettings,
  shareFirst,
  sourcesFocus,
  onOpenShareDialog,
  onManageSources,
  onChatsChanged,
}: {
  project: Project;
  chats: ConversationSummary[];
  // The signed-in user — decides who may edit or retire a team learning
  // (its author, or the project owner) without a second round trip.
  userEmail: string;
  isOwner: boolean;
  onBack: () => void;
  onOpenChat: (conversationId: string) => void;
  // Open a TEAMMATE's shared chat in the read-only viewer.
  onOpenTeamChat: (conversationId: string) => void;
  // Start a chat in this project. `afterCreate` runs once the conversation
  // exists and before it opens — how "New chat · Shared with <team>" shares
  // it (a chat with no messages yet can still be shared).
  onNewChat: (afterCreate?: (conversationId: string) => Promise<void>) => void;
  // Both mutations resolve true on success — the Instructions card and the
  // share-project confirm keep their state (and the parent toasts the error)
  // on failure. Here onUpdateSettings only ever shares the project; renames,
  // making it personal, transfer and delete live in ProjectSettingsDialog.
  onSaveInstructions: (instructions: string) => Promise<boolean>;
  onUpdateSettings: (patch: {
    name?: string;
    team_shared?: boolean;
  }) => Promise<boolean>;
  // The viewer's own team (#1157): "" = not in a team, so team sharing cannot
  // work yet and the getting-started card says where to fix that.
  // undefined = not read yet — the copy stays neutral.
  myTeam?: string;
  // The gear and the visibility pill's "Project settings" ask the parent,
  // which renders ProjectSettingsDialog (B26–B31) — the one settings surface.
  onOpenSettings: () => void;
  // "Share project first" from the share dialog's A2 state (B14/B15): open
  // with the share-the-project confirm already up, naming the chat the owner
  // came from as the next step.
  shareFirst?: { conversationId: string; title: string };
  // "Manage in Sources" / a toast's "Manage": open and scroll to this chat's
  // Sources group.
  sourcesFocus?: string;
  // The full share dialog for one of the caller's chats ("More sharing
  // options…").
  onOpenShareDialog?: (conversationId: string) => void;
  // Reopen this home at a chat's Sources group after it has closed (a toast's
  // "Manage" outlives the page that raised it).
  onManageSources?: (conversationId: string) => void;
  // A chat's sharing changed here; the parent re-reads its conversation list.
  onChatsChanged?: () => void;
}) {
  const { notify } = useChatToast();
  // Server-side chat list with previews; the prop list (already in client
  // state) renders instantly while this loads, then the previews fill in.
  const [fetchedChats, setFetchedChats] = useState<ProjectChatEntry[] | null>(null);
  const [teamChats, setTeamChats] = useState<TeamChatEntry[] | null>(null);
  // A failed team-chats read is REPORTED, not rendered as
  // "Nothing shared by your teammates yet" — the two look identical to a
  // reader and only one of them is true.
  const [teamChatsError, setTeamChatsError] = useState<string | null>(null);
  // Search over both chat lists (Item E1). Client-side over lists already in
  // memory: a project's chats are bounded by what one member filed there, and
  // the point is finding a chat you know is here, fast.
  const [query, setQuery] = useState("");
  const searchInputId = useId();

  // Instructions draft — resets when the saved value changes (React's
  // "reset state when a prop changes" render-time pattern, same as the rail's
  // rename-nonce; project switches remount via the parent's key=).
  const [draft, setDraft] = useState(project.instructions ?? "");
  const [savedInstructions, setSavedInstructions] = useState(
    project.instructions ?? "",
  );
  const [savingInstructions, setSavingInstructions] = useState(false);
  if ((project.instructions ?? "") !== savedInstructions) {
    setSavedInstructions(project.instructions ?? "");
    setDraft(project.instructions ?? "");
  }
  const dirty = draft !== savedInstructions;

  // Chat list with previews — best-effort; failure keeps the prop list.
  //
  // Re-runs when the LIVE list for this project changes (a chat dragged in
  // from the rail, one moved out, a share toggled), not only on project.id.
  // Fetched once, this panel showed a snapshot for as long as it stayed open:
  // file a chat into the project you are looking at and it simply did not
  // appear until you left and came back.
  const liveKey = chats
    .map((c) => `${c.id}:${c.title}:${c.team_visible ? 1 : 0}:${c.share_token ? 1 : 0}`)
    .join("|");
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/conversations`,
            { cache: "no-store" },
          );
          if (!res.ok) return;
          const data = (await res.json()) as {
            conversations?: ProjectChatEntry[];
          };
          if (!cancelled) setFetchedChats(data.conversations ?? null);
        } catch {
          // keep the prop list
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id, liveKey]);

  // The Team section (Item C3). Only a team-shared project can have one — a
  // personal project's chats cannot be team-shared at all — so the fetch is
  // skipped rather than asking for a list that is structurally empty.
  const teamShared = Boolean(project.team_id);
  // C-9: the owner is no longer in the team this project is shared with —
  // usually because an admin moved them, which unshared their chats but left
  // the PROJECT pointed at the old team (only the owner can re-point it).
  // `myTeam === undefined` means the team read has not landed, so the page
  // stays quiet rather than accusing.
  const strandedFromTeam =
    isOwner && teamShared && myTeam !== undefined && myTeam !== project.team_id;
  useEffect(() => {
    if (!teamShared) return;
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/projects/${encodeURIComponent(project.id)}/team-conversations`,
            { cache: "no-store" },
          );
          if (!res.ok) {
            if (!cancelled) {
              setTeamChats([]);
              setTeamChatsError(`Couldn’t load your team’s shared chats (HTTP ${res.status}).`);
            }
            return;
          }
          const data = (await res.json()) as { conversations?: TeamChatEntry[] };
          if (!cancelled) {
            setTeamChats(data.conversations ?? []);
            setTeamChatsError(null);
          }
        } catch {
          if (!cancelled) {
            setTeamChats([]);
            setTeamChatsError("Couldn’t reach the server to list your team’s shared chats.");
          }
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id, teamShared]);

  // ── Sharing on the project home (Fleet Projects sharing) ──────────────────
  //
  // The project's team ("" = personal) and the viewer's own team, as names.
  // A personal project is shared with the viewer's team, so the card and the
  // share-first confirm name THAT team before the project carries one.
  const teamName = project.team_id ?? "";
  const shareTeam = teamName || myTeam || "";

  // Per-person, per-project UI state: getting-started dismissal ("Keep
  // personal"), whether this person has shared a chat here, and which Sources
  // groups they keep open. A failed read is not "fresh state": the card then
  // falls back to the live chat list (any chat of theirs already shared here
  // means they are past it), and Sources to its own defaults.
  const [myState, setMyState] = useState<ProjectMyState | null>(null);
  const [myStateFailed, setMyStateFailed] = useState(false);
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const d = await fetchProjectMyState(project.id);
          if (cancelled) return;
          setMyState({
            kept_personal: Boolean(d?.kept_personal),
            has_shared_chat: Boolean(d?.has_shared_chat),
            sources_open: d?.sources_open ?? {},
          });
        } catch {
          if (!cancelled) setMyStateFailed(true);
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [project.id]);

  // X on the card hides it for now (this visit), not for good.
  const [cardHidden, setCardHidden] = useState(false);
  // Flipped the moment a share from this page lands, so the card goes without
  // waiting for a my-state re-read.
  const [sharedHere, setSharedHere] = useState(false);
  const [newChatMenuOpen, setNewChatMenuOpen] = useState(false);
  const newChatAnchorRef = useRef<HTMLButtonElement | null>(null);
  const instructionsId = useId();
  const [sourcesReloadKey, setSourcesReloadKey] = useState(0);
  // A share changed somewhere else (the share dialog opened from a row's
  // "More sharing options…", the rail): the live list's key moves, and
  // Sources re-reads so its groups' Only you / <team> state and counts follow.
  const [seenLiveKey, setSeenLiveKey] = useState(liveKey);
  if (liveKey !== seenLiveKey) {
    setSeenLiveKey(liveKey);
    setSourcesReloadKey((k) => k + 1);
  }
  const [busy, setBusy] = useState<{ shareProject?: boolean; shareReadyChat?: boolean }>({});

  // "Share project first" (B14/B15). The confirm opens on arrival; after it,
  // step 3 of the card names the chat the owner came from. If the project is
  // somehow already shared, there is nothing to confirm — go straight to
  // naming the chat.
  const shareFirstChat = shareFirst
    ? { id: shareFirst.conversationId, title: shareFirst.title }
    : null;
  const [shareFirstConfirm, setShareFirstConfirm] = useState<{
    id: string;
    title: string;
  } | null>(() => (shareFirstChat && !project.team_id ? shareFirstChat : null));
  const [readyChat, setReadyChat] = useState<{ id: string; title: string } | null>(
    () => (shareFirstChat && project.team_id ? shareFirstChat : null),
  );
  const [seenShareFirst, setSeenShareFirst] = useState<string | null>(
    shareFirst?.conversationId ?? null,
  );
  if ((shareFirst?.conversationId ?? null) !== seenShareFirst) {
    setSeenShareFirst(shareFirst?.conversationId ?? null);
    if (shareFirstChat) {
      if (project.team_id) setReadyChat(shareFirstChat);
      else setShareFirstConfirm(shareFirstChat);
    }
  }

  // Sources focus: from the prop (the share dialog's "Manage in Sources")
  // and from this page's own toasts. The nonce makes a second "Manage" for
  // the same chat a new request.
  const [sourcesFocusState, setSourcesFocusState] = useState<SourcesFocus | null>(
    sourcesFocus ? { conversationId: sourcesFocus, nonce: 1 } : null,
  );
  const [seenSourcesFocus, setSeenSourcesFocus] = useState<string | null>(
    sourcesFocus ?? null,
  );
  if ((sourcesFocus ?? null) !== seenSourcesFocus) {
    setSeenSourcesFocus(sourcesFocus ?? null);
    if (sourcesFocus) {
      setSourcesFocusState((f) => ({
        conversationId: sourcesFocus,
        nonce: (f?.nonce ?? 0) + 1,
      }));
    }
  }
  // A toast outlives this page (opening a chat unmounts it), so its "Manage"
  // asks the parent to reopen the home when the page is gone.
  const mountedRef = useRef(false);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);
  const manage = useCallback(
    (conversationId: string) => {
      if (mountedRef.current) {
        setSourcesFocusState((f) => ({ conversationId, nonce: (f?.nonce ?? 0) + 1 }));
      } else {
        onManageSources?.(conversationId);
      }
    },
    [onManageSources],
  );

  const onSourcesOpenChange = useCallback(
    (next: Record<string, boolean>) => {
      setMyState((s) => (s ? { ...s, sources_open: next } : s));
      void updateProjectMyState(project.id, { sources_open: next }).catch(() => {
        // Remembering open groups is a convenience; the click already took.
      });
    },
    [project.id],
  );

  // shareChat is the one fast path every share on this page takes (row pill,
  // card, New chat · Shared): it shares every output and confirms the files
  // in a toast whose "Manage" opens Sources at the chat (B5, #36).
  const shareChat = useCallback(
    async (id: string, title: string, team: string): Promise<boolean> => {
      try {
        const r = await shareChatWithTeam(id, true);
        notify(sharedToast(title || "Untitled", team, r.shared_files, () => manage(id)));
        return true;
      } catch (err) {
        notify({
          message: `Couldn’t share “${title || "Untitled"}” with ${team}. ${
            err instanceof Error ? err.message : ""
          }`.trim(),
        });
        return false;
      }
    },
    [notify, manage],
  );

  const patchChatShared = (id: string, visible: boolean) => {
    setFetchedChats((prev) =>
      prev ? prev.map((c) => (c.id === id ? { ...c, team_visible: visible } : c)) : prev,
    );
    setSourcesReloadKey((k) => k + 1);
    onChatsChanged?.();
  };

  const shareFromHere = async (chat: { id: string; title: string }) => {
    const ok = await shareChat(chat.id, chat.title, teamName);
    if (!ok) return;
    setSharedHere(true);
    setReadyChat((r) => (r && r.id === chat.id ? null : r));
    patchChatShared(chat.id, true);
  };

  const unshareFromHere = async (chat: { id: string; title: string }) => {
    const title = chat.title || "Untitled";
    try {
      const r = await shareChatWithTeam(chat.id, false);
      const n = r.shared_files;
      notify({
        message: `“${title}” is Only you again${
          n > 0
            ? `, and ${plural(n, "shared file", "shared files")} stopped being shared.`
            : "."
        }`,
      });
      patchChatShared(chat.id, false);
    } catch (err) {
      notify({
        message: `Couldn’t stop sharing “${title}”. ${
          err instanceof Error ? err.message : ""
        }`.trim(),
      });
    }
  };

  // New chat · Only you is today's path; New chat · Shared with <team>
  // creates the chat and shares it before it opens.
  const startChat = (shared: boolean) => {
    if (!shared || !teamName) {
      onNewChat();
      return;
    }
    const team = teamName;
    onNewChat(async (id) => {
      // A brand-new chat has no outputs yet, so the B5 toast's file count
      // would always be zero; the prototype's own sentence says it better.
      try {
        await shareChatWithTeam(id, true);
        notify({
          message: `New chat shared with ${team}. Files it creates will be shared too.`,
        });
      } catch (err) {
        notify({
          message: `Couldn’t share the new chat with ${team}. ${
            err instanceof Error ? err.message : ""
          }`.trim(),
        });
      }
    });
  };

  const shareProject = async (thenReady?: { id: string; title: string }) => {
    setBusy((b) => ({ ...b, shareProject: true }));
    const ok = await onUpdateSettings({ team_shared: true });
    setBusy((b) => ({ ...b, shareProject: false }));
    if (!ok) return;
    if (thenReady) {
      setReadyChat(thenReady);
      setCardHidden(false);
      notify({
        message: `${project.name} is shared with ${shareTeam}. Now share your chat from the card.`,
      });
    } else {
      notify({
        message: `${project.name} is shared with ${shareTeam}. Chats stay Only you.`,
      });
    }
  };

  const keepPersonal = () => {
    setMyState((s) => ({
      kept_personal: true,
      has_shared_chat: s?.has_shared_chat ?? false,
      sources_open: s?.sources_open ?? {},
    }));
    notify({
      message: `Got it. ${project.name} stays personal, and the card won’t come back.`,
    });
    void updateProjectMyState(project.id, { kept_personal: true }).catch(() => {
      notify({ message: "Couldn’t save that choice. The card may come back next time." });
    });
  };

  const saveInstructions = async () => {
    if (savingInstructions || !dirty) return;
    setSavingInstructions(true);
    const ok = await onSaveInstructions(draft);
    setSavingInstructions(false);
    if (ok) setSavedInstructions(draft);
  };

  // Fetched list (with previews) once it lands; the prop list until then.
  const chatList: ProjectChatEntry[] = fetchedChats ?? chats;

  const q = query.trim().toLowerCase();
  const visibleChats = useMemo(
    () =>
      q
        ? chatList.filter(
            (c) =>
              (c.title ?? "").toLowerCase().includes(q) ||
              (c.preview ?? "").toLowerCase().includes(q),
          )
        : chatList,
    [chatList, q],
  );
  const visibleTeamChats = useMemo(
    () =>
      q
        ? (teamChats ?? []).filter(
            (c) =>
              (c.title ?? "").toLowerCase().includes(q) ||
              c.user_email.toLowerCase().includes(q),
          )
        : (teamChats ?? []),
    [teamChats, q],
  );
  const searchable = chatList.length + (teamChats?.length ?? 0) > 0;

  // ── Getting-started card (B6 / B8 / B12 / B25) ────────────────────────────
  // Per person, per project, until they have shared a chat here. A chat of
  // theirs already shared here counts too: people who shared before the card
  // existed are past it, whatever my-state says.
  const iSharedHere = chatList.some((c) => c.team_visible);
  const hasShared = sharedHere || Boolean(myState?.has_shared_chat) || iSharedHere;
  // The chat "Share project first" brought the owner here, while it is still
  // Only you.
  const readyEntry =
    readyChat && !chatList.some((c) => c.id === readyChat.id && c.team_visible)
      ? {
          id: readyChat.id,
          title: chatList.find((c) => c.id === readyChat.id)?.title || readyChat.title,
        }
      : null;
  // "Keep personal" is for good — except that sharing the project from the
  // share dialog's "Share project first" is an explicit change of mind.
  const keptPersonal = Boolean(myState?.kept_personal) && !readyEntry;
  const showCard =
    (myState !== null || myStateFailed) &&
    // The variant depends on the viewer's team: wait for that read.
    (Boolean(teamName) || myTeam !== undefined) &&
    // Only the owner can act on a personal project.
    (Boolean(teamName) || isOwner) &&
    !hasShared &&
    !cardHidden &&
    !keptPersonal;
  const cardModel = showCard
    ? buildGettingStarted({
        projectName: project.name,
        projectTeam: teamName,
        myTeam: myTeam ?? "",
        isOwner,
        hasInstructions: Boolean(savedInstructions.trim()),
        myChatCount: chatList.length,
        myPrivateChatCount: chatList.filter((c) => !c.team_visible).length,
        readyChat: readyEntry,
        busy,
      })
    : null;

  const onCardAction = (kind: StepActionKind) => {
    switch (kind) {
      case "add-instructions": {
        const el = document.getElementById(instructionsId);
        el?.scrollIntoView?.({ block: "center", behavior: "smooth" });
        el?.focus();
        return;
      }
      case "new-chat":
        if (teamName) {
          newChatAnchorRef.current?.scrollIntoView?.({ block: "nearest" });
          setNewChatMenuOpen(true);
        } else {
          startChat(false);
        }
        return;
      case "share-project":
        void shareProject();
        return;
      case "share-ready-chat":
        if (!readyEntry) return;
        setBusy((b) => ({ ...b, shareReadyChat: true }));
        void shareFromHere(readyEntry).finally(() =>
          setBusy((b) => ({ ...b, shareReadyChat: false })),
        );
        return;
    }
  };

  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4 sm:px-8"
      data-testid="project-home"
    >
      <div className="mx-auto max-w-5xl">
        {/* Header: back · title (+pin) · who sees it · settings */}
        <div className="mb-4">
          {/* Wraps on a phone: the title keeps a readable width and the
              visibility pill + gear drop to a second line, instead of the
              pill squeezing the project name down to two letters. */}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <button
              type="button"
              aria-label="Back to chat"
              className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={onBack}
            >
              <Icon name="arrow-right" className="size-4 rotate-180" />
            </button>
            <Icon
              name="briefcase"
              className="size-5 shrink-0 text-[var(--color-accent)]"
            />
            <h1 className="min-w-0 flex-1 basis-[9rem] truncate text-[1.35rem] font-semibold text-[var(--color-text-primary)] sm:flex-initial sm:basis-auto">
              {project.name}
            </h1>
            {project.pinned ? (
              <Icon
                name="pin"
                className="size-4 shrink-0 text-[var(--color-accent)]"
              />
            ) : null}
            {/* The pill NAMES the team (C-9): an owner an admin has since
                moved to another team must still see which team this project
                points at. */}
            <ProjectVisibilityPill
              teamName={teamName}
              isOwner={isOwner}
              onOpenSettings={onOpenSettings}
            />
            <span className="flex-1" />
            {isOwner ? (
              <button
                type="button"
                aria-label="Project settings"
                title="Project settings"
                className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                onClick={onOpenSettings}
              >
                <Icon name="settings" className="size-4" />
              </button>
            ) : null}
          </div>

          {/* C-9: the one line that explains the state, on the page rather
              than buried in a chat's share dialog. Shown only to the owner
              (nobody else can act on it) and only once the team read has
              landed. */}
          {strandedFromTeam ? (
            <p className="mt-1.5 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
              {myTeam
                ? `You’re no longer in ${project.team_id}. Share this project with ${myTeam} instead, or make it personal.`
                : `You’re no longer in ${project.team_id}, and you aren’t in a team now. Make this project personal, or set a team in Settings → Team and share it with that.`}
            </p>
          ) : null}
        </div>

        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
          {/* Main column: getting started + this member's chats + the team's. */}
          <div className="min-w-0">
            {cardModel ? (
              <ProjectGettingStarted
                model={cardModel}
                onAction={onCardAction}
                onDismiss={() => setCardHidden(true)}
                onKeepPersonal={keepPersonal}
              />
            ) : null}

            {searchable ? (
              <div className="mb-3 flex items-center gap-2 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5">
                <Icon
                  name="search"
                  className="size-3.5 shrink-0 text-[var(--color-text-muted)]"
                />
                <input
                  id={searchInputId}
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="Search chats in this project…"
                  aria-label="Search chats in this project"
                  className="min-w-0 flex-1 bg-transparent text-[0.83rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)]"
                />
                {query ? (
                  <button
                    type="button"
                    aria-label="Clear search"
                    className="shrink-0 text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
                    onClick={() => setQuery("")}
                  >
                    <Icon name="close" className="size-3.5" />
                  </button>
                ) : null}
              </div>
            ) : null}

            <YourChatsSection
              chats={chatList}
              visibleChats={visibleChats}
              query={query}
              teamName={teamName}
              showEmptyText={!cardModel}
              newChatMenuOpen={newChatMenuOpen}
              onNewChatMenuOpenChange={setNewChatMenuOpen}
              newChatAnchorRef={newChatAnchorRef}
              onNewChat={startChat}
              onOpenChat={onOpenChat}
              onShare={(c) => void shareFromHere(c)}
              onUnshare={(c) => void unshareFromHere(c)}
              onMoreOptions={(id) => onOpenShareDialog?.(id)}
            />

            {teamShared ? (
              <TeamChatsSection
                teamChats={teamChats}
                visibleTeamChats={visibleTeamChats}
                error={teamChatsError}
                query={query}
                onOpenTeamChat={onOpenTeamChat}
              />
            ) : null}
          </div>

          {/* Right column: the two team-level context layers as a pair
              (Instructions + Team learnings), then sources. */}
          <div className="flex min-w-0 flex-col gap-4">
            <div className={cardClass}>
              <div className="mb-2 flex items-center justify-between">
                <h2 className="text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
                  Instructions
                </h2>
                {isOwner && dirty ? (
                  <button
                    type="button"
                    disabled={savingInstructions}
                    className="rounded-md bg-[var(--color-accent)] px-2.5 py-1 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-90 disabled:opacity-60"
                    onClick={() => void saveInstructions()}
                  >
                    {savingInstructions ? "Saving…" : "Save"}
                  </button>
                ) : null}
              </div>
              {isOwner ? (
                <textarea
                  id={instructionsId}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  rows={8}
                  maxLength={8000}
                  placeholder="Standing instructions for every chat in this project…"
                  aria-label="Project instructions"
                  className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-2.5 text-[0.83rem] leading-relaxed text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)]"
                />
              ) : (
                <p className="whitespace-pre-wrap text-[0.83rem] leading-relaxed text-[var(--color-text-secondary)]">
                  {project.instructions?.trim() || "No instructions set."}
                </p>
              )}
              {/* The three layers, in the order buildSystemPrompt assembles
                  them. Instructions really do come first; team learnings and
                  personal memories arrive together in the memories block, the
                  project's tagged [project] — so this says "then", not
                  "before personal memories", which named only two of three. */}
              <p className="mt-1.5 text-[0.7rem] leading-[1.5] text-[var(--color-text-muted)]">
                Every chat here is fed by three layers:{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  Instructions
                </strong>{" "}
                first (owner-only), then this project&rsquo;s{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  Team learnings
                </strong>{" "}
                and each member&rsquo;s own{" "}
                <strong className="font-medium text-[var(--color-text-secondary)]">
                  My memory
                </strong>
                .
              </p>
            </div>

            <TeamLearningsPanel
              projectId={project.id}
              projectOwner={project.owner_email}
              userEmail={userEmail}
              teamShared={teamShared}
            />

            <ProjectSources
              projectId={project.id}
              teamName={teamName}
              reloadKey={sourcesReloadKey}
              focus={sourcesFocusState}
              sourcesOpen={myState?.sources_open}
              onSourcesOpenChange={onSourcesOpenChange}
            />
          </div>
        </div>
      </div>

      {/* B14: "Share project first" lands with this confirm already open.
          Cancel changes nothing; confirming shares the PROJECT only — the
          chat stays Only you until the card's one-click "Share it". */}
      {shareFirstConfirm ? (
        <ConfirmDialog
          title={`Share ${project.name} with ${shareTeam || "your team"}?`}
          confirmLabel={`Share with ${shareTeam || "your team"}`}
          confirmTone="accent"
          busy={busy.shareProject}
          testId="share-project-first-confirm"
          onCancel={() => setShareFirstConfirm(null)}
          onConfirm={() => {
            const chat = shareFirstConfirm;
            setShareFirstConfirm(null);
            void shareProject(chat);
          }}
        >
          <p className="m-0">
            {shareTeam || "Your team"} will see {project.name}&rsquo;s
            instructions and Team learnings. Each chat stays Only you until you
            share it.
          </p>
          <p className="m-0">
            Next, you can share &ldquo;{shareFirstConfirm.title || "Untitled"}
            &rdquo;.
          </p>
        </ConfirmDialog>
      ) : null}
    </div>
  );
}

// ── Team learnings (Item D2) ─────────────────────────────────────────────────
//
// The project's shared memory, and the FIRST surface anywhere that shows it:
// before this the entries were written (by the agent, by the projects modal)
// and injected into every project chat, with no screen listing them. Each row
// carries its writer and date, because a learning nobody can attribute is a
// rumour.
//
// Permissions, in one line: members manage their own entries, the owner
// manages all, and Retire is the default remove — it stops the entry being
// injected while keeping the record of what was learned and by whom. Delete is
// there for a genuine mistake. The server re-checks both (a hidden button is
// honest UI, not enforcement).
export function TeamLearningsPanel({
  projectId,
  projectOwner,
  userEmail,
  teamShared,
}: {
  projectId: string;
  projectOwner: string;
  userEmail: string;
  teamShared: boolean;
}) {
  const [entries, setEntries] = useState<TeamLearning[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState("");

  const load = useCallback(async () => {
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories`,
        { cache: "no-store" },
      );
      if (!res.ok) throw new Error(await res.text());
      const data = (await res.json()) as { memories?: TeamLearning[] };
      setEntries(data.memories ?? []);
      setError(null);
    } catch {
      // entries stays NULL, not []: an empty array renders "No team learnings
      // yet. Save one from any chat in this project" underneath the error,
      // which is a claim about the project we just failed to read.
      setEntries(null);
      setError("Couldn’t load team learnings.");
    }
  }, [projectId]);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void load();
    });
    return () => {
      cancelled = true;
    };
  }, [load]);

  const [savingEdit, setSavingEdit] = useState(false);
  // The entry a delete confirm is up for. A DIALOG, not an inline "Delete for
  // good · Keep" swap: the swap was easy to miss (the second click was
  // permanent with no dialog anywhere), and worse, it persisted — retiring an
  // entry left the row reading "Restore · Delete for good · Keep" with no
  // delete pending at all, one click from destroying someone's contribution
  // while "Keep" did nothing. This state is cleared by every write.
  const [confirmRemoveId, setConfirmRemoveId] = useState<string | null>(null);

  const canManage = (m: TeamLearning) =>
    (m.user_email ?? "").toLowerCase() === userEmail.toLowerCase() ||
    projectOwner.toLowerCase() === userEmail.toLowerCase();

  const add = async () => {
    const content = draft.trim();
    if (!content || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ content }),
        },
      );
      if (!res.ok) throw new Error(await res.text());
      setDraft("");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t save that learning.");
    } finally {
      setBusy(false);
    }
  };

  // Returns whether the write landed, so a caller holding unsaved text (the
  // inline editor) knows whether it may throw that text away.
  const patch = async (id: string, body: Record<string, unknown>): Promise<boolean> => {
    setError(null);
    // Any action clears a pending delete: the confirm belongs to the click
    // that opened it and to nothing else.
    setConfirmRemoveId(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(id)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
      );
      if (!res.ok) throw new Error(await res.text());
      await load();
      return true;
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t update that learning.");
      return false;
    }
  };

  const remove = async (id: string) => {
    setError(null);
    setConfirmRemoveId(null);
    try {
      const res = await fetch(
        `/api/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(id)}`,
        { method: "DELETE" },
      );
      if (!res.ok) throw new Error(await res.text());
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Couldn’t delete that learning.");
    }
  };

  const actionClass =
    "rounded px-1 py-0.5 text-[0.68rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]";

  // Pinned first, the server's order otherwise. Array#sort is stable, so two
  // pinned entries keep their relative order — pinning is a promotion, not a
  // reshuffle.
  const ordered = useMemo(
    () =>
      entries
        ? [...entries].sort(
            (a, b) => Number(Boolean(b.pinned)) - Number(Boolean(a.pinned)),
          )
        : null,
    [entries],
  );
  const pendingRemoval = ordered?.find((m) => m.id === confirmRemoveId) ?? null;

  return (
    <div className={cardClass} data-testid="team-learnings">
      <h2 className="mb-2 flex items-center gap-1.5 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
        <Icon name="brain" className="size-3.5 shrink-0" />
        Team learnings
      </h2>
      {error ? (
        <p className="mb-2 text-[0.72rem] text-[var(--color-danger)]">{error}</p>
      ) : null}
      {entries === null ? (
        <p className="text-[0.8rem] text-[var(--color-text-muted)]">
          {error ? "" : "Loading…"}
        </p>
      ) : entries.length === 0 ? (
        <p className="text-[0.8rem] leading-[1.5] text-[var(--color-text-muted)]">
          No team learnings yet. Save one from any chat in this project — every
          chat here is told about them.
        </p>
      ) : (
        <ul className="m-0 grid list-none gap-2 p-0">
          {(ordered ?? []).map((m) => {
            const retired = Boolean(m.retired_at);
            return (
              <li
                key={m.id}
                className="group border-b border-[var(--color-border)] pb-2 last:border-b-0 last:pb-0"
              >
                {editingId === m.id ? (
                  <div className="grid gap-1">
                    <textarea
                      value={editDraft}
                      onChange={(e) => setEditDraft(e.target.value)}
                      rows={3}
                      aria-label="Edit team learning"
                      className="w-full resize-y rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-2 text-[0.8rem] leading-relaxed text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)]"
                    />
                    <div className="flex justify-end gap-2">
                      <button
                        type="button"
                        className={actionClass}
                        onClick={() => setEditingId(null)}
                      >
                        Cancel
                      </button>
                      <button
                        type="button"
                        className={actionClass}
                        disabled={!editDraft.trim() || savingEdit}
                        onClick={() => {
                          const content = editDraft.trim();
                          if (!content) return;
                          if (content === m.content) {
                            setEditingId(null);
                            return;
                          }
                          // The editor stays up until the write lands. Tearing
                          // it down first threw the typed text away on any
                          // rejection — a permission error, a 500 — leaving a
                          // one-line message and no way back to the rewrite.
                          setSavingEdit(true);
                          void patch(m.id, { content }).then((ok) => {
                            setSavingEdit(false);
                            if (ok) setEditingId(null);
                          });
                        }}
                      >
                        {savingEdit ? "Saving…" : "Save"}
                      </button>
                    </div>
                  </div>
                ) : (
                  <>
                    <p
                      className={[
                        "m-0 whitespace-pre-wrap text-[0.8rem] leading-[1.5]",
                        retired
                          ? "text-[var(--color-text-muted)] line-through"
                          : "text-[var(--color-text-secondary)]",
                      ].join(" ")}
                    >
                      {m.content}
                    </p>
                    {/* Provenance line, and the row's ONE action affordance.
                        Five text links per entry was heavy for a panel that
                        will hold dozens — and put an irreversible Delete one
                        word from Retire. Pinned is a glyph here, beside
                        author · date, rather than a marker in the sentence,
                        and the entry sorts to the top. */}
                    <div className="mt-1 flex flex-wrap items-center gap-2 text-[0.68rem] text-[var(--color-text-muted)]">
                      {m.pinned ? (
                        // Labelled: the glyph is the ONLY thing saying this
                        // entry is pinned now that "Pin/Unpin" moved into the
                        // menu, and Icon renders aria-hidden.
                        <span
                          role="img"
                          aria-label="Pinned"
                          title="Pinned"
                          className="flex shrink-0 items-center text-[var(--color-accent)]"
                        >
                          <Icon name="pin" className="size-3" />
                        </span>
                      ) : null}
                      <span>
                        {m.user_email ? shortName(m.user_email) : "unknown"}
                        {m.created_at ? ` · ${formatDay(m.created_at)}` : ""}
                        {retired ? " · retired" : ""}
                      </span>
                      {canManage(m) ? (
                        <span className="ml-auto flex items-center">
                          <LearningActions
                            entry={m}
                            retired={retired}
                            onPin={() => void patch(m.id, { pinned: !m.pinned })}
                            onEdit={() => {
                              setConfirmRemoveId(null);
                              setEditingId(m.id);
                              setEditDraft(m.content);
                            }}
                            onRetire={() =>
                              void patch(m.id, { retired: !retired })
                            }
                            onDelete={() => setConfirmRemoveId(m.id)}
                          />
                        </span>
                      ) : null}
                    </div>
                  </>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <div className="mt-3 flex items-center gap-2">
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void add();
          }}
          placeholder="Add a learning the whole project should know…"
          aria-label="New team learning"
          className="min-w-0 flex-1 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1.5 text-[0.78rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)]"
        />
        <button
          type="button"
          disabled={busy || !draft.trim()}
          className="shrink-0 rounded-md border border-[var(--color-border-strong)] px-2.5 py-1.5 text-[0.72rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-40"
          onClick={() => void add()}
        >
          Add
        </button>
      </div>
      {!teamShared ? (
        <p className="mt-2 text-[0.68rem] leading-[1.5] text-[var(--color-text-muted)]">
          This project isn&rsquo;t shared with a team yet, so these are yours
          alone — they still reach every chat in the project.
        </p>
      ) : null}
      {pendingRemoval ? (
        <ConfirmDialog
          title="Delete this team learning for good?"
          confirmLabel="Delete for good"
          cancelLabel="Keep"
          confirmTone="danger"
          layer="stacked"
          onCancel={() => setConfirmRemoveId(null)}
          onConfirm={() => void remove(pendingRemoval.id)}
        >
          <p className="m-0 whitespace-pre-wrap text-[var(--color-text-primary)]">
            {pendingRemoval.content}
          </p>
          <p className="m-0">
            {pendingRemoval.user_email
              ? `Written by ${shortName(pendingRemoval.user_email)}. `
              : ""}
            The record of what was learned goes with it. To stop it being
            injected while keeping the record, retire it instead.
          </p>
        </ConfirmDialog>
      ) : null}
    </div>
  );
}

// LearningActions — one overflow menu per team learning, matching how a chat
// row's ⋮ works everywhere else in fleet (the same shared Menu surface and
// keyboard contract). Revealed on hover AND on keyboard focus: the button
// stays in the tab order at opacity 0, and the row's group-focus-within brings
// it into view when it, or anything else in the row, is focused.
function LearningActions({
  entry,
  retired,
  onPin,
  onEdit,
  onRetire,
  onDelete,
}: {
  entry: TeamLearning;
  retired: boolean;
  onPin: () => void;
  onEdit: () => void;
  onRetire: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const close = () => setOpen(false);
  // A snippet, not the id: with several rows on screen the accessible name has
  // to say WHICH learning this menu acts on.
  const snippet =
    entry.content.length > 40 ? `${entry.content.slice(0, 40)}…` : entry.content;
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Actions for “${snippet}”`}
        title="Actions"
        className={[
          "inline-flex size-[1.6rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open
            ? "opacity-100"
            : "opacity-0 focus-visible:opacity-100 group-hover:opacity-100 group-focus-within:opacity-100",
        ].join(" ")}
        onClick={() => setOpen((o) => !o)}
      >
        <Icon name="dots" className="size-4" />
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={`Actions for team learning “${snippet}”`}
        className="min-w-[10rem]"
      >
        <MenuItem
          icon={<Icon name="pin" className="size-4" />}
          onClick={() => {
            close();
            onPin();
          }}
        >
          {entry.pinned ? "Unpin" : "Pin"}
        </MenuItem>
        <MenuItem
          icon={<Icon name="edit" className="size-4" />}
          onClick={() => {
            close();
            onEdit();
          }}
        >
          Edit
        </MenuItem>
        <MenuItem
          icon={<Icon name={retired ? "refresh" : "stop"} className="size-4" />}
          description={
            retired
              ? "Use this learning again"
              : "Stop using it; keep the record"
          }
          onClick={() => {
            close();
            onRetire();
          }}
        >
          {retired ? "Restore" : "Retire"}
        </MenuItem>
        <MenuSeparator />
        <MenuItem
          danger
          icon={<Icon name="trash" className="size-4" />}
          onClick={() => {
            close();
            onDelete();
          }}
        >
          Delete
        </MenuItem>
      </Menu>
    </>
  );
}
