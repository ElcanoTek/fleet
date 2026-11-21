import { t as tr } from "@/app/lib/i18n";
// One chat on Settings → Admin → Chats: who owns it and what it is waiting
// on, the conversation as its owner sees it (read-only), the ways an
// administrator can step in, its patrols, its settings, and the record of
// what administrators have done to it.

import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { copyText } from "@/app/shared/lib/clipboard";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { useVisibleInterval } from "@/app/shared/hooks/useVisibleInterval";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

import { getGatedJson } from "../../lib/getGated";
import { ConnBadge, FormActions, MissingFieldsHint } from "../../ui/atoms";
import { ConnEmpty, ConnPanel } from "../../ui/panels";
import { formatAgo } from "../lib";
import { AdminTranscript } from "./AdminTranscript";
import {
  actionLabel,
  approvalModeLabel,
  chatUrl,
  formatSpan,
  initials,
  type AdminAction,
  type AdminChat,
  type ChatDetailData,
  type PendingCard,
  type TranscriptEntry,
} from "./api";
import { ChatSettingsPanel, LifecyclePanel, ReasonField, TransferPanel } from "./ManagePanels";
import { PatrolPanel } from "./PatrolPanel";
import { PinnedNoteCard, Status, StepIn, postRequest, useRequestFollower, type StepInMode } from "./StepIn";

// How many entries one read of the conversation asks for, and how much of a
// long entry it carries.
const PAGE = 40;
const MAX_CHARS = 20000;
// How often an open chat is re-read while its tab is visible, so a running
// turn is seen as it lands.
export const DETAIL_POLL_MS = 8000;
// How often the waiting times shown for approval cards are brought up to date.
const CLOCK_TICK_MS = 30_000;

// What a read answers for a chat that no longer exists; never shown.
const GONE = "admin-chat-gone";

type Tab = "conversation" | "patrols" | "settings" | "activity";

function historyUrl(chatId: string, withTools: boolean, before?: number): string {
  const q = new URLSearchParams({ tail: String(PAGE), max_chars: String(MAX_CHARS) });
  if (withTools) q.set("tools", "1");
  if (before) q.set("before", String(before));
  return `${chatUrl(chatId, "/history")}?${q.toString()}`;
}

// mergeLatest joins a fresh read of the latest entries onto what is already on
// screen (earlier pages the reader loaded, then the previous latest page).
//
// The fresh page replaces everything from its first entry on, so an entry
// that changed or went away since the last read is shown as it now stands.
// It only joins when it reaches back into what is shown. If it starts after
// the last entry on screen, entries in between were never read (more than a
// page arrived between two reads) or the earlier ones are gone (the chat was
// truncated): the pages on screen are then dropped rather than shown with a
// silent hole, and the reader can load earlier messages again.
export function mergeLatest(shown: TranscriptEntry[], fresh: TranscriptEntry[]): { entries: TranscriptEntry[]; joined: boolean } {
  if (shown.length === 0 || fresh.length === 0) return { entries: fresh, joined: shown.length === 0 };
  const firstFresh = fresh[0].id;
  if (firstFresh > shown[shown.length - 1].id) return { entries: fresh, joined: false };
  return { entries: [...shown.filter((e) => e.id < firstFresh), ...fresh], joined: true };
}

export function ChatDetail({ chatId, onBack, onChanged }: { chatId: string; onBack: () => void; onChanged: () => void }) {
  const [detail, setDetail] = useState<ChatDetailData | null>(null);
  // Every entry on screen, oldest first: earlier pages the reader loaded,
  // then the latest page. null until the first read lands.
  const [shown, setShown] = useState<TranscriptEntry[] | null>(null);
  // The chat was deleted for good while this view was open.
  const [gone, setGone] = useState(false);
  const [moreOlder, setMoreOlder] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [withTools, setWithTools] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("conversation");
  const [mode, setMode] = useState<StepInMode>("note");
  // Bumped to read the chat again: by the poll, by a change made here, by
  // the refresh button.
  const [tick, setTick] = useState(0);
  const refresh = useCallback(() => setTick((t) => t + 1), []);

  // One read of the chat and its latest entries. Reads overlap (the poll, a
  // change, the tool-calls switch) and can land out of order; a read that has
  // been superseded is dropped.
  useEffect(() => {
    let stale = false;
    const missing = { 404: GONE };
    Promise.all([
      getGatedJson<ChatDetailData>(chatUrl(chatId), { label: tr("Chat"), statusText: missing }),
      getGatedJson<{ entries: TranscriptEntry[] }>(historyUrl(chatId, withTools), { label: tr("Conversation"), statusText: missing }),
    ])
      .then(([d, h]) => {
        if (stale) return;
        if (d) setDetail(d);
        if (h) {
          const full = h.entries.length >= PAGE;
          setShown((prev) => {
            const merged = mergeLatest(prev ?? [], h.entries);
            // Joined: whether earlier entries remain is what it was. Not
            // joined (first read, or a hole): a full page means there may be.
            if (!merged.joined || prev === null) setMoreOlder(full);
            return merged.entries;
          });
        }
        setError(null);
      })
      .catch((err: unknown) => {
        if (stale) return;
        // Deleted for good (by this administrator a moment ago, or by
        // another): there is nothing left to show or to poll.
        if (err instanceof Error && err.message === GONE) setGone(true);
        else setError(errorMessage(err, tr("Couldn’t load this chat.")));
      });
    return () => {
      stale = true;
    };
  }, [chatId, withTools, tick]);
  useVisibleInterval(refresh, DETAIL_POLL_MS, !gone);

  const changed = useCallback(() => {
    refresh();
    onChanged();
  }, [refresh, onChanged]);

  // Entries paged back to belong to one choice of the tool-calls switch.
  const switchTools = (next: boolean) => {
    setWithTools(next);
    setShown(null);
    setMoreOlder(false);
  };

  const loadOlder = async () => {
    const first = shown?.[0]?.id;
    if (!first) return;
    setLoadingOlder(true);
    try {
      const h = await getGatedJson<{ entries: TranscriptEntry[] }>(historyUrl(chatId, withTools, first), { label: tr("Conversation") });
      if (h) {
        // Only onto the page they were asked for: a refresh may have dropped
        // the pages on screen meanwhile.
        // Whether more remain is a fact about the page that was applied, so
        // it is set only with it: a page that arrived too late must not hide
        // "Load earlier messages" for the pages that replaced the ones it
        // belonged to.
        setShown((prev) => {
          if (!prev || prev[0]?.id !== first) return prev;
          setMoreOlder(h.entries.length >= PAGE);
          return [...h.entries, ...prev];
        });
      }
    } catch (err) {
      setError(errorMessage(err, tr("Couldn’t load earlier messages.")));
    } finally {
      setLoadingOlder(false);
    }
  };

  const back = (
    <Button type="button" variant="ghost" size="sm" className="-ml-2 mb-3 border-transparent" onClick={onBack} data-testid="admin-chat-back">
      <Icon name="chevron-right" className="size-3.5 rotate-180" />
      {tr("All chats")}
    </Button>
  );
  if (gone) {
    return (
      <div data-testid="admin-chat-gone">
        {back}
        <ConnEmpty>{tr("This chat no longer exists. It was deleted permanently.")}</ConnEmpty>
      </div>
    );
  }
  if (error && !detail) {
    return (
      <div>
        {back}
        <NoticeBanner tone="danger">{error}</NoticeBanner>
      </div>
    );
  }
  if (!detail) {
    return (
      <div>
        {back}
        <p className="text-sm text-[var(--color-text-muted)]">{tr("Loading…")}</p>
      </div>
    );
  }
  const c = detail.chat;
  const binned = Boolean(c.deleted_at);
  const note = detail.note ?? null;
  const cards = detail.pending_cards ?? [];
  const entries = shown ?? [];
  const livePatrols = detail.patrols.filter((p) => p.status === "active" || p.status === "paused");
  return (
    <div data-testid="admin-chat-panel">
      {back}
      <ChatHeader chat={c} onChanged={changed} />
      {error ? <NoticeBanner tone="danger" className="mt-3">{error}</NoticeBanner> : null}
      {binned ? null : <AttentionCallout chat={c} cards={cards} />}

      <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)} className="mt-5 gap-4">
        <TabsList variant="line" className="w-full justify-start overflow-x-auto">
          <TabsTrigger value="conversation" className="max-sm:px-2">{tr("Conversation")}</TabsTrigger>
          <TabsTrigger value="patrols" className="max-sm:px-2" data-testid="admin-tab-patrols">
            {tr("Patrols")}
            {livePatrols.length > 0 ? <TabCount n={livePatrols.length} /> : null}
          </TabsTrigger>
          <TabsTrigger value="settings" className="max-sm:px-2" data-testid="admin-tab-settings">{tr("Settings")}</TabsTrigger>
          <TabsTrigger value="activity" className="max-sm:px-2" data-testid="admin-tab-activity">
            {tr("Activity")}
            {detail.actions.length > 0 ? <TabCount n={detail.actions.length} /> : null}
          </TabsTrigger>
        </TabsList>

        <TabsContent value="conversation" className="grid gap-3">
          {note ? (
            <PinnedNoteCard
              note={note}
              onEdit={() => {
                setMode("note");
                document.querySelector<HTMLTextAreaElement>('[data-testid="admin-note-text"]')?.focus();
              }}
            />
          ) : null}
          <ConversationFrame
            chat={c}
            entries={entries}
            loaded={shown !== null}
            withTools={withTools}
            onWithTools={switchTools}
            moreOlder={moreOlder}
            loadingOlder={loadingOlder}
            onLoadOlder={() => void loadOlder()}
            onRefresh={refresh}
          />
          {binned ? null : <StepIn chat={c} note={note} mode={mode} onMode={setMode} onChanged={changed} />}
        </TabsContent>

        <TabsContent value="patrols">
          {binned ? (
            <ConnEmpty>{tr("A chat in the recycle bin runs no patrols. Restore it to manage them.")}</ConnEmpty>
          ) : detail.patrols.length === 0 ? (
            <ConnEmpty>{tr("This chat has no patrols or timers.")}</ConnEmpty>
          ) : (
            detail.patrols.map((p) => <PatrolPanel key={p.id} patrol={p} onDone={changed} />)
          )}
        </TabsContent>

        <TabsContent value="settings">
          {binned ? null : (
            <>
              <ChatSettingsPanel key={`${c.title}|${c.model}|${c.approval_mode}`} chat={c} onDone={changed} />
              <TransferPanel chat={c} onDone={changed} />
            </>
          )}
          <LifecyclePanel chat={c} onDone={changed} />
        </TabsContent>

        <TabsContent value="activity">
          <ActivityList actions={detail.actions} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

function TabCount({ n }: { n: number }) {
  return (
    <span className="rounded-full bg-[var(--color-overlay-soft)] px-1.5 text-[0.68rem] font-medium leading-[1.5] text-[var(--color-text-secondary)]">
      {n}
    </span>
  );
}

function ChatHeader({ chat, onChanged }: { chat: AdminChat; onChanged: () => void }) {
  const [stopping, setStopping] = useState(false);
  const busyWork = chat.turn_running || chat.queued_inputs > 0;
  return (
    <header>
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <h3 className="m-0 min-w-0 flex-1 basis-64 text-[1.2rem] font-semibold leading-[1.3] tracking-[-0.01em] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
          {chat.title || tr("Untitled chat")}
        </h3>
        {busyWork && !chat.deleted_at ? (
          <Button type="button" variant="outline" size="sm" aria-expanded={stopping} onClick={() => setStopping((v) => !v)} data-testid="admin-chat-stop">
            <Icon name="stop" className="size-3.5" />
            {stopping ? tr("Cancel") : tr("Stop running work")}
          </Button>
        ) : null}
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[0.8rem] text-[var(--color-text-secondary)] max-sm:gap-x-3">
        <Avatar className="size-6 ring-0">
          <AvatarFallback className="size-6 text-[0.62rem] ring-0">{initials(chat.owner_email)}</AvatarFallback>
        </Avatar>
        <span className="min-w-0 font-medium text-[var(--color-text-primary)] [overflow-wrap:anywhere]">{chat.owner_email}</span>
        <Dot />
        <span className="min-w-0 [overflow-wrap:anywhere]">{chat.model || tr("default model")}</span>
        <Dot />
        <span>{tr("Approvals: {0}", [approvalModeLabel(chat.approval_mode)])}</span>
        <Dot />
        <span>{tr("active {0}", [formatAgo(chat.updated_at)])}</span>
      </div>
      <div className="mt-2.5 flex flex-wrap items-center gap-1.5">
        <ChatBadges chat={chat} />
        <IdChip id={chat.id} />
      </div>
      {chat.multiplayer && (chat.participants?.length ?? 0) > 0 ? (
        <p className="mb-0 mt-2 text-[0.75rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
          {tr("Multiplayer with {0}", [(chat.participants ?? []).join(", ")])}
        </p>
      ) : null}
      {stopping ? <StopForm chat={chat} onDone={onChanged} /> : null}
    </header>
  );
}

// IdChip shows the start of the chat's id and copies the whole of it: the id
// is what `fleet chats` on the box takes.
function IdChip({ id }: { id: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      title={tr("Copy the chat id")}
      aria-label={tr("Copy the chat id")}
      onClick={() => {
        void copyText(id).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        });
      }}
      className="inline-flex items-center gap-1.5 rounded-[var(--radius-pill)] px-1.5 py-0.5 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
      data-testid="admin-chat-id"
    >
      {copied ? tr("Copied") : id.slice(0, 8)}
      <Icon name={copied ? "check" : "copy"} className="size-3 shrink-0" />
    </button>
  );
}

function Dot() {
  return (
    <span aria-hidden="true" className="text-[var(--color-text-muted)] max-sm:hidden">
      ·
    </span>
  );
}

/** ChatBadges is a chat's state at a glance; the list rows and the header share it. */
export function ChatBadges({ chat }: { chat: AdminChat }) {
  return (
    <>
      {chat.turn_running ? <ConnBadge variant="success">{tr("running")}</ConnBadge> : null}
      {chat.pending_approvals > 0 ? (
        <ConnBadge variant="warn">{chat.pending_approvals === 1 ? tr("1 card waiting") : tr("{0} cards waiting", [chat.pending_approvals])}</ConnBadge>
      ) : null}
      {chat.last_turn_status === "error" ? <ConnBadge variant="warn">{tr("last turn failed")}</ConnBadge> : null}
      {chat.queued_inputs > 0 ? <ConnBadge>{tr("{0} queued", [chat.queued_inputs])}</ConnBadge> : null}
      {chat.active_patrols > 0 ? (
        <ConnBadge>{chat.active_patrols === 1 ? tr("1 patrol") : tr("{0} patrols", [chat.active_patrols])}</ConnBadge>
      ) : null}
      {chat.has_note ? <ConnBadge variant="overridden">{chat.note_seen ? tr("note seen") : tr("note pinned")}</ConnBadge> : null}
      {chat.multiplayer ? <ConnBadge>{tr("Multiplayer")}</ConnBadge> : null}
      {chat.archived ? <ConnBadge>{tr("archived")}</ConnBadge> : null}
      {chat.lockdown ? <ConnBadge variant="overridden">{tr("Lockdown")}</ConnBadge> : null}
      {chat.purges_at ? (
        <ConnBadge variant="warn">{tr("deleted · gone {0}", [new Date(chat.purges_at * 1000).toLocaleDateString()])}</ConnBadge>
      ) : null}
    </>
  );
}

function StopForm({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const { status, failed, follow, setStatus, setFailed } = useRequestFollower(onDone);
  const hintId = useId();
  const stop = async () => {
    setBusy(true);
    setStatus(null);
    try {
      const req = await postRequest(chat.id, { kind: "stop", reason });
      if (req) follow(req);
    } catch (err) {
      setStatus(errorMessage(err, tr("The change failed")));
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  return (
    <ConnPanel className="mb-0 mt-3">
      <p className="mb-2.5 mt-0 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
        {tr("This is {0}’s own Stop: the running turn ends and everything queued behind it is cancelled.", [chat.owner_email])}
      </p>
      <ReasonField value={reason} onChange={setReason} testId="admin-stop-reason" />
      <FormActions className="mt-3" hint={<MissingFieldsHint id={hintId} missing={reason.trim() ? [] : [tr("Reason")]} />}>
        <Button type="button" size="sm" variant="destructive" disabled={busy || !reason.trim()} aria-describedby={hintId} onClick={() => void stop()}>
          {tr("Stop now")}
        </Button>
      </FormActions>
      <div className="mt-2">
        <Status tone={failed ? "error" : "ok"} text={status} />
      </div>
    </ConnPanel>
  );
}

// AttentionCallout says what the chat's owner is stuck behind. An
// administrator cannot answer someone else's approval card (ADR-0115); what
// they can do is point the owner at it, which is what the note is for.
function AttentionCallout({ chat, cards }: { chat: AdminChat; cards: PendingCard[] }) {
  // "Waiting 3h" and "denied unanswered in 20h" are read off a clock that
  // keeps time while the view stays open, not the moment it was first drawn.
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000));
  useVisibleInterval(() => setNow(Math.floor(Date.now() / 1000)), CLOCK_TICK_MS);
  const failed = chat.last_turn_status === "error";
  if (cards.length === 0 && !failed) return null;
  return (
    <div
      role="status"
      className="mt-4 rounded-[var(--radius-lg)] border border-[var(--color-warning-border)] px-3.5 py-3"
      data-testid="admin-chat-attention"
    >
      <p className="m-0 flex items-center gap-2 text-[0.84rem] font-semibold text-[var(--color-text-primary)]">
        <Icon name="alert-triangle" className="size-4 shrink-0 text-[var(--color-warning-text)]" />
        {cards.length > 0 ? tr("Waiting on {0}", [chat.owner_email]) : tr("The last turn ended in an error")}
      </p>
      {cards.length > 0 ? (
        <>
          <p className="mb-0 mt-1.5 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
            {tr("Only people in the chat can answer an approval card. A pinned note is the surest way to point them at it.")}
            {failed ? ` ${tr("The last turn also ended in an error.")}` : ""}
          </p>
          <ul className="m-0 mt-2 grid list-none gap-1 p-0">
            {cards.map((card) => (
              <li key={card.id} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-[0.78rem] text-[var(--color-text-secondary)]">
                <code className="min-w-0 rounded-[var(--radius-sm)] bg-[var(--color-overlay-soft)] px-1.5 py-0.5 font-[family-name:var(--font-code)] text-[0.72rem] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                  {card.tool}
                </code>
                <span>
                  {tr("waiting {0}", [formatSpan(now - card.created_at)])}
                  {card.expires_at && card.expires_at > now ? tr(" · denied unanswered in {0}", [formatSpan(card.expires_at - now)]) : ""}
                  {card.for_email && card.for_email.toLowerCase() !== chat.owner_email.toLowerCase() ? tr(" · for {0}", [card.for_email]) : ""}
                </span>
              </li>
            ))}
          </ul>
          {chat.pending_approvals > cards.length ? (
            <p className="mb-0 mt-1.5 text-[0.75rem] text-[var(--color-text-muted)]" data-testid="attention-more-cards">
              {tr("These are the {0} that have waited longest; {1} more are waiting.", [cards.length, chat.pending_approvals - cards.length])}
            </p>
          ) : null}
        </>
      ) : (
        <p className="mb-0 mt-1.5 text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)]">
          {tr("{0} may not know why. The conversation below shows where it stopped; a note or a message can get them going again.", [chat.owner_email])}
        </p>
      )}
    </div>
  );
}

function ConversationFrame({
  chat,
  entries,
  loaded,
  withTools,
  onWithTools,
  moreOlder,
  loadingOlder,
  onLoadOlder,
  onRefresh,
}: {
  chat: AdminChat;
  entries: TranscriptEntry[];
  loaded: boolean;
  withTools: boolean;
  onWithTools: (v: boolean) => void;
  moreOlder: boolean;
  loadingOlder: boolean;
  onLoadOlder: () => void;
  onRefresh: () => void;
}) {
  const box = useRef<HTMLDivElement | null>(null);
  const lastId = entries[entries.length - 1]?.id ?? 0;
  const firstId = entries[0]?.id ?? 0;
  const seen = useRef({ lastId: 0, firstId: 0, height: 0 });
  // Open at the end, like the chat. Follow new entries only while the reader
  // is at the end; keep their place when earlier messages are added above.
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    const prev = seen.current;
    const atEnd = prev.height - el.scrollTop - el.clientHeight < 48;
    if (prev.lastId === 0 || (lastId !== prev.lastId && atEnd)) {
      el.scrollTop = el.scrollHeight;
    } else if (firstId !== prev.firstId && prev.firstId !== 0) {
      el.scrollTop += el.scrollHeight - prev.height;
    }
    seen.current = { lastId, firstId, height: el.scrollHeight };
  }, [lastId, firstId]);
  return (
    <section
      aria-label={tr("Conversation, as {0} sees it", [chat.owner_email])}
      className="overflow-hidden rounded-[var(--radius-lg)] border border-[var(--color-border)]"
    >
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1.5 border-b border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3.5 py-2">
        <span className="flex min-w-0 items-center gap-1.5 text-[0.75rem] text-[var(--color-text-secondary)]">
          <Icon name="lock" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 [overflow-wrap:anywhere]">{tr("As {0} sees it · read-only", [chat.owner_email])}</span>
        </span>
        <span className="flex items-center gap-3">
          <label className="flex items-center gap-1.5 text-[0.75rem] text-[var(--color-text-secondary)]">
            <input type="checkbox" checked={withTools} onChange={(e) => onWithTools(e.target.checked)} data-testid="admin-chat-tools" />
            {tr("Show tool calls")}
          </label>
          <Button type="button" variant="ghost" size="icon-xs" aria-label={tr("Refresh")} title={tr("Refresh")} onClick={onRefresh}>
            <Icon name="refresh" className="size-3.5" />
          </Button>
        </span>
      </div>
      {/* A scrolling region must be reachable by keyboard to be scrolled by it. */}
      {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex */}
      <div ref={box} role="group" aria-label={tr("Conversation")} className="max-h-[32rem] min-h-[10rem] overflow-y-auto px-4 py-4" tabIndex={0} data-testid="admin-chat-scroll">
        {withTools && chat.multiplayer ? (
          // In a Multiplayer chat each person sees their own turns' tool
          // calls, not the others'. This switch shows all of them.
          <p className="mb-4 mt-0 text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]" data-testid="admin-chat-tools-note">
            {tr("Tool calls from every participant’s turns are shown. In the chat, each person sees only their own.")}
          </p>
        ) : null}
        {moreOlder && entries.length > 0 ? (
          <div className="mb-4 flex justify-center">
            <Button type="button" variant="ghost" size="xs" disabled={loadingOlder} onClick={onLoadOlder} data-testid="admin-chat-older">
              {loadingOlder ? tr("Loading…") : tr("Load earlier messages")}
            </Button>
          </div>
        ) : null}
        {!loaded ? (
          <p className="m-0 text-sm text-[var(--color-text-muted)]">{tr("Loading…")}</p>
        ) : entries.length === 0 ? (
          <p className="m-0 py-6 text-center text-[0.82rem] text-[var(--color-text-muted)]">{tr("Nothing has been said in this chat yet.")}</p>
        ) : (
          <AdminTranscript entries={entries} owner={chat.owner_email} />
        )}
        {chat.turn_running ? (
          <p className="mb-0 mt-4 flex items-center gap-2 text-[0.78rem] text-[var(--color-text-muted)]" data-testid="admin-chat-running">
            <span className="inline-block size-2 animate-pulse rounded-full bg-[var(--color-success)]" />
            {tr("The assistant is working…")}
          </p>
        ) : null}
      </div>
    </section>
  );
}

function ActivityList({ actions }: { actions: AdminAction[] }) {
  if (actions.length === 0) {
    return <ConnEmpty>{tr("No administrator has changed this chat.")}</ConnEmpty>;
  }
  return (
    <ol className="m-0 grid list-none gap-0 p-0" data-testid="admin-chat-activity">
      {actions.map((a) => (
        <li key={a.id} className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 border-b border-[var(--color-border-subtle)] py-2.5 last:border-b-0">
          <span className="mt-[0.35rem] inline-block size-2 rounded-full bg-[var(--color-accent)]" aria-hidden="true" />
          <div className="min-w-0">
            <p className="m-0 flex flex-wrap items-baseline justify-between gap-x-3 text-[0.84rem] font-medium text-[var(--color-text-primary)]">
              <span>{actionLabel(a.action)}</span>
              <span className="text-[0.72rem] font-normal text-[var(--color-text-muted)]">{formatAgo(a.created_at)}</span>
            </p>
            <p className="mb-0 mt-0.5 text-[0.75rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
              {a.via === "cli" ? tr("{0} · from the command line", [a.actor]) : tr("{0} · in the browser", [a.actor])}
            </p>
            {a.reason ? (
              <p className="mb-0 mt-1 text-[0.78rem] text-[var(--color-text-secondary)] [overflow-wrap:anywhere]">{tr("Reason: {0}", [a.reason])}</p>
            ) : null}
            {a.detail ? (
              <p className="mb-0 mt-1 line-clamp-3 whitespace-pre-wrap text-[0.75rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">{a.detail}</p>
            ) : null}
          </div>
        </li>
      ))}
    </ol>
  );
}
