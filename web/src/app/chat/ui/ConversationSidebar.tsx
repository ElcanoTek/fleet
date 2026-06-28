"use client";

// Conversation sidebar extracted verbatim from chat-experience.tsx (slice 6
// of the #169 decomposition). This is the left-hand <aside>: branding/header,
// the new-chat / new-sealed-chat / search / close affordances, the email
// label, the cross-view orchestrator link, the searchable conversation list
// (each row's open/download/archive/delete/pin controls), the collapsible
// Archived section, and the footer (update-available banner, "Delete all
// unpinned", sign-out form).
//
// It is purely presentational + event-forwarding: every piece of mutable
// state and every handler still lives in ChatExperience and is threaded in
// through props. Nothing here reaches into ChatExperience's closure. The move
// is a pure relocation — markup, class names, aria-labels, titles, and the
// per-row "Pin/Unpin/Archive/Unarchive/Delete <title>" accessible names are
// byte-identical to the in-module originals (the live conversation-mgmt e2e
// spec depends on them).

import type { Dispatch, RefObject, SetStateAction } from "react";
import Image from "next/image";
import { NavToOrchestrator } from "@/app/shared/ui/CrossViewNav";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { Icon } from "./Icon";
import type {
  ConversationSummary,
  PendingDeleteConversation,
  ServerConfig,
} from "./chat-experience";

export function ConversationSidebar({
  sidebarOpen,
  setSidebarOpen,
  branding,
  serverConfig,
  clearConversation,
  setSearchOpen,
  setShortcutsOpen,
  userEmail,
  sidebarQuery,
  setSidebarQuery,
  searchRef,
  searchShortcut,
  isLoadingHistory,
  filteredConversations,
  activeConversationId,
  loadConversation,
  streamingConvs,
  downloadConversation,
  toggleArchive,
  setPendingDeleteConversation,
  togglePin,
  archivedConversations,
  showArchived,
  setShowArchived,
  updateAvailable,
  conversations,
  setConfirmBulkDelete,
}: {
  sidebarOpen: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
  branding: ClientBranding;
  serverConfig: ServerConfig;
  clearConversation: (opts?: { lockdown?: boolean }) => void;
  setSearchOpen: Dispatch<SetStateAction<boolean>>;
  setShortcutsOpen: Dispatch<SetStateAction<boolean>>;
  userEmail: string;
  sidebarQuery: string;
  setSidebarQuery: Dispatch<SetStateAction<string>>;
  searchRef: RefObject<HTMLInputElement | null>;
  searchShortcut: string;
  isLoadingHistory: boolean;
  filteredConversations: ConversationSummary[];
  activeConversationId: string | null;
  loadConversation: (
    conversationId: string,
    options?: { preserveScroll?: boolean },
  ) => Promise<void>;
  streamingConvs: Set<string>;
  downloadConversation: (conversation: ConversationSummary) => Promise<void>;
  toggleArchive: (conversation: ConversationSummary, archived: boolean) => Promise<void>;
  setPendingDeleteConversation: Dispatch<SetStateAction<PendingDeleteConversation | null>>;
  togglePin: (conversation: ConversationSummary) => Promise<void>;
  archivedConversations: ConversationSummary[];
  showArchived: boolean;
  setShowArchived: Dispatch<SetStateAction<boolean>>;
  updateAvailable: boolean;
  conversations: ConversationSummary[];
  setConfirmBulkDelete: Dispatch<SetStateAction<boolean>>;
}) {
  return (
    <aside
      className={[
        // Mobile: 85vw-capped drawer that leaves a thumb-sized strip on
        // the right so users can swipe-dismiss or see they're in an
        // overlay. safe-area-inset-left so it doesn't slide under an
        // iPhone notch / rounded corner.
        "fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(19rem,85vw)] flex-col overflow-auto border-r border-[var(--color-border)] bg-[color-mix(in_srgb,var(--sidebar-surface)_96%,black)] px-3 py-4 shadow-[var(--shadow-lg)] backdrop-blur-xl transition-transform duration-200 sm:w-[min(17rem,calc(100vw-2.5rem))] sm:bg-[var(--sidebar-surface)] lg:sticky lg:h-screen lg:w-auto lg:translate-x-0 lg:border-r-0 lg:bg-[var(--sidebar-surface)] lg:shadow-none lg:backdrop-blur-0",
        sidebarOpen ? "translate-x-0" : "-translate-x-full",
      ].join(" ")}
      style={{
        paddingLeft: "max(0.75rem, env(safe-area-inset-left))",
        paddingBottom: "max(1rem, env(safe-area-inset-bottom))",
      }}
    >
      <div className="mb-4 flex items-center justify-between px-1">
        <a className="flex items-center gap-2.5 no-underline" href="#">
          <Image
            src="/logos/elcano-mark-primary.svg"
            alt={branding.app_name}
            width={28}
            height={28}
            priority
          />
          <span className="font-heading text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">
            {branding.app_name}
          </span>
        </a>
        <div className="flex items-center gap-1">
          {/* Three modes:
              - lockdownOnly: only the lockdown +button (every
                chat is forcibly lockdown).
              - lockdownAvailable && !lockdownOnly: both buttons.
              - !lockdownAvailable: only the regular +button (no
                sandbox image configured, lockdown unsupported). */}
          <button
            className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7"
            type="button"
            title="Search conversations (⌘K)"
            aria-label="Search conversations"
            onClick={() => setSearchOpen(true)}
          >
            <Icon name="search" className="size-4" />
          </button>
          <button
            className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7"
            type="button"
            title="Keyboard shortcuts (?)"
            aria-label="Keyboard shortcuts"
            data-testid="shortcuts-button"
            onClick={() => setShortcutsOpen(true)}
          >
            <Icon name="info" className="size-4" />
          </button>
          {!serverConfig.lockdownOnly ? (
            <button
              className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7"
              type="button"
              title="New chat"
              aria-label="New chat"
              onClick={() => clearConversation()}
            >
              <Icon name="plus" className="size-4" />
            </button>
          ) : null}
          {serverConfig.lockdownAvailable ? (
            <button
              className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-accent)] sm:size-7"
              type="button"
              title={
                serverConfig.lockdownOnly
                  ? "New chat — every chat on this server is sealed (sandboxed, vetted model only)"
                  : "New sealed chat — sandboxed, vetted model, nothing leaves"
              }
              aria-label={
                serverConfig.lockdownOnly
                  ? "New chat — every chat on this server is sealed (sandboxed, vetted model only)"
                  : "New sealed chat — sandboxed, vetted model, nothing leaves"
              }
              onClick={() => clearConversation({ lockdown: true })}
            >
              <Icon name="lock" className="size-4" />
            </button>
          ) : null}
          <button
            aria-label="Close sidebar"
            className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] sm:size-7 lg:hidden"
            type="button"
            onClick={() => setSidebarOpen(false)}
          >
            <svg
              aria-hidden="true"
              className="size-4"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M18 6 6 18" />
              <path d="m6 6 12 12" />
            </svg>
          </button>
        </div>
      </div>

      <div className="mb-2 px-2 text-[0.75rem] text-[var(--color-text-muted)]">
        {userEmail || "Loading..."}
      </div>

      {/* Cross-view link to the orchestrator (View B), from the shared
          shell. The one middleware gates both /chat and /orchestrator off
          the same session, so this navigates without a re-login. */}
      <NavToOrchestrator className="mb-4 block rounded-md px-2 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]" />

      <div className="flex-1 overflow-y-auto">
        <p className="mb-1 px-2 text-[0.6875rem] font-medium text-[var(--color-text-muted)]">
          Conversations
        </p>
        <div className="mb-1 px-2">
          <input
            ref={searchRef}
            type="search"
            value={sidebarQuery}
            onChange={(e) => setSidebarQuery(e.target.value)}
            placeholder={`Search chats (${searchShortcut})`}
            className="w-full rounded-md border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
          />
        </div>

        {isLoadingHistory ? (
          <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">Loading...</p>
        ) : filteredConversations.length === 0 ? (
          <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">
            {sidebarQuery.trim() ? "No matches." : "No saved chats yet."}
          </p>
        ) : (
          filteredConversations.map((conversation) => (
            <div
              key={conversation.id}
              className={[
                "group relative rounded-md transition",
                activeConversationId === conversation.id
                  ? "bg-[var(--color-overlay-soft)]"
                  : "hover:bg-[var(--color-overlay-soft)]",
              ].join(" ")}
            >
              <button
                className={[
                  "block w-full min-w-0 rounded-md px-2 py-1.5 pr-44 text-left text-[0.8125rem] transition sm:pr-[8rem]",
                  activeConversationId === conversation.id
                    ? "text-[var(--color-text-primary)]"
                    : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
                ].join(" ")}
                type="button"
                onClick={() => void loadConversation(conversation.id)}
              >
                <span className="flex min-w-0 items-center gap-1.5">
                  {streamingConvs.has(conversation.id) ? (
                    <span
                      aria-label="Working"
                      title="Working…"
                      className="inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--color-accent)]"
                    />
                  ) : null}
                  {conversation.lockdown ? (
                    <Icon
                      name="lock"
                      className="size-3 shrink-0 text-[var(--color-accent)]"
                    />
                  ) : null}
                  <span className="block truncate">{conversation.title}</span>
                </span>
              </button>

              <div className="touch-reveal pointer-events-none absolute inset-y-0 right-1 flex items-center gap-1 opacity-0 transition group-hover:opacity-100 group-focus-within:opacity-100">
                <button
                  aria-label={`Download ${conversation.title} as JSON`}
                  title="Download as JSON"
                  className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                  type="button"
                  onClick={() => void downloadConversation(conversation)}
                >
                  <Icon name="download" className="size-3.5" />
                </button>
                <button
                  aria-label={`Archive ${conversation.title}`}
                  title="Archive"
                  className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                  type="button"
                  onClick={() => void toggleArchive(conversation, true)}
                >
                  <svg
                    aria-hidden="true"
                    viewBox="0 0 24 24"
                    className="size-3.5"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth={1.8}
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  >
                    <rect x="3" y="4" width="18" height="4" rx="1" />
                    <path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" />
                    <path d="M10 12h4" />
                  </svg>
                </button>
                <button
                  aria-label={`Delete ${conversation.title}`}
                  className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                  type="button"
                  onClick={() =>
                    setPendingDeleteConversation({ id: conversation.id, title: conversation.title })
                  }
                >
                  <Icon name="trash" className="size-3.5" />
                </button>
                <button
                  aria-label={conversation.pinned ? `Unpin ${conversation.title}` : `Pin ${conversation.title}`}
                  title={conversation.pinned ? "Unpin" : "Pin"}
                  className={[
                    "pointer-events-auto inline-flex size-10 items-center justify-center rounded-md transition hover:bg-[var(--color-overlay-strong)] sm:size-7",
                    conversation.pinned
                      ? "text-[var(--color-accent)]"
                      : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]",
                  ].join(" ")}
                  type="button"
                  onClick={() => void togglePin(conversation)}
                >
                  <svg
                    aria-hidden="true"
                    viewBox="0 0 24 24"
                    className="size-3.5"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth={conversation.pinned ? 2.2 : 1.8}
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  >
                    <path d="M12 17v5" />
                    <path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z" />
                  </svg>
                </button>
              </div>
            </div>
          ))
        )}

        {archivedConversations.length > 0 ? (
          <div className="mt-3 border-t border-[var(--color-border)] pt-2">
            <button
              type="button"
              aria-expanded={showArchived}
              aria-label={`Archived conversations (${archivedConversations.length})`}
              className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-[0.6875rem] font-medium text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)]"
              onClick={() => setShowArchived((v) => !v)}
            >
              <Icon
                name={showArchived ? "chevron-down" : "chevron-right"}
                className="size-3 shrink-0"
              />
              Archived ({archivedConversations.length})
            </button>
            {showArchived
              ? archivedConversations.map((conversation) => (
                  <div
                    key={conversation.id}
                    className={[
                      "group relative rounded-md transition",
                      activeConversationId === conversation.id
                        ? "bg-[var(--color-overlay-soft)]"
                        : "hover:bg-[var(--color-overlay-soft)]",
                    ].join(" ")}
                  >
                    <button
                      className="block w-full min-w-0 rounded-md py-1.5 pl-7 pr-24 text-left text-[0.8125rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)] sm:pr-[4.5rem]"
                      type="button"
                      onClick={() => void loadConversation(conversation.id)}
                    >
                      <span className="block truncate italic">{conversation.title}</span>
                    </button>

                    <div className="touch-reveal pointer-events-none absolute inset-y-0 right-1 flex items-center gap-1 opacity-0 transition group-hover:opacity-100 group-focus-within:opacity-100">
                      <button
                        aria-label={`Unarchive ${conversation.title}`}
                        title="Unarchive"
                        className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                        type="button"
                        onClick={() => void toggleArchive(conversation, false)}
                      >
                        <svg
                          aria-hidden="true"
                          viewBox="0 0 24 24"
                          className="size-3.5"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth={1.8}
                          strokeLinecap="round"
                          strokeLinejoin="round"
                        >
                          <rect x="3" y="4" width="18" height="4" rx="1" />
                          <path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" />
                          <path d="M12 18v-6" />
                          <path d="M9.5 14.5 12 12l2.5 2.5" />
                        </svg>
                      </button>
                      <button
                        aria-label={`Delete ${conversation.title}`}
                        className="touch-reveal pointer-events-auto inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                        type="button"
                        onClick={() =>
                          setPendingDeleteConversation({ id: conversation.id, title: conversation.title })
                        }
                      >
                        <Icon name="trash" className="size-3.5" />
                      </button>
                    </div>
                  </div>
                ))
              : null}
          </div>
        ) : null}
      </div>

      <div className="grid gap-1 pt-3">
        {updateAvailable ? (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md border border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 px-2 py-1.5 text-left text-[0.75rem] font-medium text-[var(--color-accent)] transition hover:bg-[var(--color-accent)]/20 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={() => window.location.reload()}
            title="A newer version of the app has been deployed. Click to refresh and load it."
          >
            <Icon name="refresh" className="size-3.5 shrink-0" />
            Chat has been updated — click to refresh
          </button>
        ) : null}
        {conversations.some((c) => !c.pinned) ? (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.75rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-danger)]"
            onClick={() => setConfirmBulkDelete(true)}
            title="Delete every unpinned conversation"
          >
            <Icon name="trash" className="size-3.5 shrink-0" />
            Delete all unpinned
          </button>
        ) : null}
        <form action="/api/auth/logout" method="post">
          <button
            className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.8125rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            type="submit"
          >
            <Icon name="logout" className="size-4 shrink-0" />
            Sign out
          </button>
        </form>
      </div>
    </aside>
  );
}
