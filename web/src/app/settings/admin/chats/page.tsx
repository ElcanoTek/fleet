import { t as tr } from "@/app/lib/i18n";
// Settings → Admin → Chats — chat administration (ADR-0115, ADR-0117).
//
// The page answers two questions in order. First "who needs help?": the
// workspace's chats counted by what they are waiting on, and grouped by the
// person who owns them, so an administrator starts from people and not from a
// list of every chat. Then, for one chat (?chat=<id>), "what is going on and
// how do I help?": the conversation as its owner sees it, read-only, and the
// ways to step in — a pinned note, a message to the assistant, a notice — plus
// its patrols, settings and the record of admin changes (ChatDetail).
//
// Every change takes a reason where it alters the chat, is recorded
// (GET /api/admin/chat-actions) and is visible in the chat. `fleet chats` on
// the box does the same from a terminal.

import { useCallback, useEffect, useState } from "react";
import { useRouter, useSearch } from "@/app/lib/navigation";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { useVisibleInterval } from "@/app/shared/hooks/useVisibleInterval";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { cn } from "@/lib/utils";

import { AdminGateFallback } from "../../AdminGateFallback";
import { getGatedJson } from "../../lib/getGated";
import { ConnBadge, SETTINGS_INPUT, Segmented } from "../../ui/atoms";
import { ConnEmpty, DirChip, DirSearch, SetSection } from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { formatAgo } from "../lib";
import { initials, type AdminChat, type ChatOverview, type ChatPerson } from "./api";
import { ChatBadges, ChatDetail } from "./ChatDetail";

export { formatRunTime, lineDiff } from "./api";
export type { AdminChat, AdminPatrol } from "./api";

// What the listing is narrowed to. The first four are the tiles ("what needs
// me?"); archived and bin are the chats a default listing leaves out.
type Filter = "all" | "attention" | "running" | "patrols" | "noted" | "archived" | "bin";
type View = "people" | "chats";

const OVERVIEW_POLL_MS = 30_000;

function listQuery(filter: Filter, query: string, days: string, owner = ""): string {
  const p = new URLSearchParams();
  if (query.trim()) p.set("q", query.trim());
  if (owner) p.set("owner", owner);
  if (days !== "0") p.set("active_days", days);
  if (filter === "bin") p.set("deleted", "1");
  else if (filter !== "all") p.set("state", filter);
  p.set("limit", "200");
  return p.toString();
}

function personCount(p: ChatPerson, filter: Filter): number {
  if (filter === "attention") return p.attention;
  if (filter === "running") return p.running;
  if (filter === "patrols") return p.patrols;
  if (filter === "noted") return p.noted;
  return p.chats;
}

export default function AdminChatsPage() {
  const router = useRouter();
  const admin = useIsAdmin();
  const search = useSearch();
  const selected = new URLSearchParams(search).get("chat");
  // Bumped when a chat changes, so the overview behind an open chat is
  // current when the administrator comes back to it.
  const [version, setVersion] = useState(0);

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  const open = useCallback((id: string) => router.push(`/settings/admin/chats?chat=${encodeURIComponent(id)}`), [router]);
  const back = useCallback(() => router.push("/settings/admin/chats"), [router]);
  const changed = useCallback(() => setVersion((v) => v + 1), []);

  if (admin !== "admin") return <AdminGateFallback state={admin} />;

  return (
    <>
      {/* The overview stays mounted behind an open chat, so its search and
          filters are as the administrator left them on the way back. */}
      <div hidden={Boolean(selected)}>
        <Overview version={version} onOpen={open} />
      </div>
      {selected ? <ChatDetail key={selected} chatId={selected} onBack={back} onChanged={changed} /> : null}
    </>
  );
}

function Overview({ version, onOpen }: { version: number; onOpen: (id: string) => void }) {
  const [query, setQuery] = useState("");
  const [days, setDays] = useState("30");
  const [filter, setFilter] = useState<Filter>("all");
  const [view, setView] = useState<View>("people");
  const [overview, setOverview] = useState<ChatOverview | null>(null);
  const [error, setError] = useState<string | null>(null);
  // The search box is debounced into `q`, which is what requests use.
  const [q, setQ] = useState("");
  useEffect(() => {
    const t = window.setTimeout(() => setQ(query), 250);
    return () => window.clearTimeout(t);
  }, [query]);

  // Bumped by the poll to read the overview, and the chat rows under it,
  // again: a chat that recovers or starts running elsewhere must leave or
  // join the list on screen, not only change the counts above it.
  const [tick, setTick] = useState(0);
  const fresh = version + tick;
  useVisibleInterval(() => setTick((t) => t + 1), OVERVIEW_POLL_MS);
  useEffect(() => {
    let stale = false;
    const p = new URLSearchParams();
    if (q.trim()) p.set("q", q.trim());
    if (days !== "0") p.set("active_days", days);
    getGatedJson<{ overview: ChatOverview }>(`/api/admin/chat-overview?${p.toString()}`, { label: tr("Chats") })
      .then((data) => {
        if (stale || !data) return;
        setOverview(data.overview);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!stale) setError(errorMessage(err, tr("Couldn’t load chats.")));
      });
    return () => {
      stale = true;
    };
  }, [q, days, version, tick]);

  const flat = view === "chats" || filter === "archived" || filter === "bin";
  const tile = (f: Filter) => () => setFilter((cur) => (cur === f ? "all" : f));
  return (
    <SetSection
      title={tr("Chats")}
      intro={tr("See who needs help and step into their chat: read it as they see it, pin a note for them, message the assistant, or fix a patrol. Everything you do is recorded and visible in the chat.")}
    >
      <div className="grid grid-cols-2 gap-[0.7rem] min-[620px]:grid-cols-4" data-testid="admin-chats-tiles">
        <Tile
          icon="alert-triangle"
          tone="warn"
          label={tr("Need attention")}
          value={overview?.attention}
          sub={overview ? (overview.cards_waiting === 1 ? tr("1 card waiting") : tr("{0} cards waiting", [overview.cards_waiting])) : ""}
          active={filter === "attention"}
          onClick={tile("attention")}
          testId="tile-attention"
        />
        <Tile icon="activity" tone="ok" label={tr("Running now")} value={overview?.running} sub={tr("a turn in progress")} active={filter === "running"} onClick={tile("running")} testId="tile-running" />
        <Tile icon="clock" label={tr("With patrols")} value={overview?.patrols} sub={tr("an active schedule")} active={filter === "patrols"} onClick={tile("patrols")} testId="tile-patrols" />
        <Tile icon="pin" label={tr("Pinned notes")} value={overview?.noted} sub={tr("a note from an admin")} active={filter === "noted"} onClick={tile("noted")} testId="tile-noted" />
      </div>

      <div className="mt-4 grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto]">
        <DirSearch value={query} onChange={setQuery} placeholder={tr("Search people, titles or chat ids")} label={tr("Search chats")} />
        <select
          className={cn(SETTINGS_INPUT, "min-h-[2.45rem] w-auto py-[0.4rem] text-[0.83rem]")}
          value={days}
          onChange={(e) => setDays(e.target.value)}
          aria-label={tr("Active in")}
        >
          <option value="7">{tr("Active in the last 7 days")}</option>
          <option value="30">{tr("Active in the last 30 days")}</option>
          <option value="90">{tr("Active in the last 90 days")}</option>
          <option value="0">{tr("Active any time")}</option>
        </select>
      </div>

      <div className="mt-3 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <div className="flex flex-wrap gap-1.5" role="group" aria-label={tr("Which chats")}>
          <DirChip active={filter !== "archived" && filter !== "bin"} ariaPressed={filter !== "archived" && filter !== "bin"} onClick={() => setFilter("all")} count={overview?.chats}>
            {tr("Open chats")}
          </DirChip>
          <DirChip active={filter === "archived"} ariaPressed={filter === "archived"} onClick={() => setFilter("archived")} count={overview?.archived}>
            {tr("Archived")}
          </DirChip>
          <DirChip active={filter === "bin"} ariaPressed={filter === "bin"} onClick={() => setFilter("bin")} count={overview?.deleted}>
            {tr("Recycle bin")}
          </DirChip>
        </div>
        {filter === "archived" || filter === "bin" ? null : (
          <Segmented<View>
            label={tr("Group by")}
            value={view}
            onChange={setView}
            options={[
              { value: "people", label: tr("By person") },
              { value: "chats", label: tr("All chats") },
            ]}
          />
        )}
      </div>

      <div className="mt-3">
        {error ? <NoticeBanner tone="danger">{error}</NoticeBanner> : null}
        {flat ? (
          <ChatList key={`${filter}|${q}|${days}`} queryString={listQuery(filter, q, days)} version={fresh} onOpen={onOpen} showOwner emptyText={emptyText(filter)} />
        ) : overview === null ? (
          error ? null : <p className="text-sm text-[var(--color-text-muted)]">{tr("Loading chats…")}</p>
        ) : (
          <>
            <PeopleList people={overview.people.filter((p) => personCount(p, filter) > 0)} filter={filter} q={q} days={days} version={fresh} onOpen={onOpen} />
            {overview.people_truncated ? (
              <p className="mb-0 mt-3 text-[0.78rem] text-[var(--color-text-muted)]" role="status" data-testid="admin-people-truncated">
                {tr("Only the first {0} people are listed; the counts above cover everyone. Search or shorten the period to find the rest.", [overview.people.length])}
              </p>
            ) : null}
          </>
        )}
      </div>
    </SetSection>
  );
}

function emptyText(filter: Filter): string {
  if (filter === "attention") return tr("Nobody is stuck: no approval card is waiting and no chat’s last turn failed.");
  if (filter === "running") return tr("No chat is running a turn right now.");
  if (filter === "patrols") return tr("No chat has an active patrol.");
  if (filter === "noted") return tr("No chat has a pinned note.");
  if (filter === "archived") return tr("No archived chats match.");
  if (filter === "bin") return tr("The recycle bin is empty.");
  return tr("No chats match.");
}

function Tile({
  icon,
  tone,
  label,
  value,
  sub,
  active,
  onClick,
  testId,
}: {
  icon: string;
  tone?: "warn" | "ok";
  label: string;
  value: number | undefined;
  sub: string;
  active: boolean;
  onClick: () => void;
  testId: string;
}) {
  const lit = (value ?? 0) > 0;
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      data-testid={testId}
      className={cn(
        "grid min-w-0 gap-1 rounded-md border px-[0.95rem] pb-[0.85rem] pt-[0.8rem] text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
        active
          ? "border-[color-mix(in_srgb,var(--color-accent)_55%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)]"
          : "border-[var(--color-border)] bg-[var(--color-surface-1)] hover:border-[var(--color-border-strong)] hover:bg-[var(--color-overlay-soft)]",
      )}
    >
      <span className="flex items-center gap-1.5 text-[0.65rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
        <Icon
          name={icon}
          className={cn(
            "size-3.5 shrink-0",
            lit && tone === "warn" ? "text-[var(--color-warning-text)]" : lit && tone === "ok" ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]",
          )}
        />
        <span className="min-w-0">{label}</span>
      </span>
      <span className="text-[1.6rem] font-semibold leading-none tracking-[-0.02em] text-[var(--color-text-primary)]">{value ?? "—"}</span>
      <span className="text-[0.72rem] text-[var(--color-text-muted)]">{sub}</span>
    </button>
  );
}

function PeopleList({
  people,
  filter,
  q,
  days,
  version,
  onOpen,
}: {
  people: ChatPerson[];
  filter: Filter;
  q: string;
  days: string;
  version: number;
  onOpen: (id: string) => void;
}) {
  const [openPerson, setOpenPerson] = useState<string | null>(null);
  if (people.length === 0) return <ConnEmpty>{emptyText(filter)}</ConnEmpty>;
  // With one person in view there is nothing to choose between: show their chats.
  const expanded = people.length === 1 ? people[0].email : openPerson;
  return (
    <ul className="m-0 grid list-none gap-2 p-0" data-testid="admin-chats-people">
      {people.map((p) => {
        const isOpen = expanded === p.email;
        const shown = personCount(p, filter);
        return (
          <li key={p.email} className="overflow-hidden rounded-md border border-[var(--color-border)] bg-[var(--color-surface-1)]">
            <button
              type="button"
              aria-expanded={isOpen}
              onClick={() => setOpenPerson(isOpen ? null : p.email)}
              className="flex w-full items-center gap-3 px-3.5 py-3 text-left hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              data-testid="admin-person"
            >
              <Avatar className="size-9 ring-0">
                <AvatarFallback className="size-9 text-[0.72rem] ring-0">{initials(p.email)}</AvatarFallback>
              </Avatar>
              <span className="grid min-w-0 flex-1 gap-1">
                <span className="truncate text-[0.9rem] font-medium text-[var(--color-text-primary)]">{p.email}</span>
                <span className="flex flex-wrap items-center gap-1.5">
                  <span className="text-[0.75rem] text-[var(--color-text-muted)]">
                    {filter === "all"
                      ? p.chats === 1
                        ? tr("1 chat · active {0}", [formatAgo(p.last_active)])
                        : tr("{0} chats · active {1}", [p.chats, formatAgo(p.last_active)])
                      : tr("{0} of {1} chats · active {2}", [shown, p.chats, formatAgo(p.last_active)])}
                  </span>
                  {p.attention > 0 ? <ConnBadge variant="warn">{p.attention === 1 ? tr("1 needs attention") : tr("{0} need attention", [p.attention])}</ConnBadge> : null}
                  {p.running > 0 ? <ConnBadge variant="success">{tr("{0} running", [p.running])}</ConnBadge> : null}
                  {p.patrols > 0 ? <ConnBadge>{p.patrols === 1 ? tr("1 with patrols") : tr("{0} with patrols", [p.patrols])}</ConnBadge> : null}
                  {p.noted > 0 ? <ConnBadge variant="overridden">{p.noted === 1 ? tr("1 note") : tr("{0} notes", [p.noted])}</ConnBadge> : null}
                </span>
              </span>
              <Icon name="chevron-right" className={cn("size-4 shrink-0 text-[var(--color-text-muted)] transition-transform", isOpen && "rotate-90")} />
            </button>
            {isOpen ? (
              <div className="border-t border-[var(--color-border)] px-2 pb-1">
                <ChatList queryString={listQuery(filter, q, days, p.email)} version={version} onOpen={onOpen} emptyText={tr("No chats match.")} />
              </div>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

function ChatList({
  queryString,
  version,
  onOpen,
  showOwner = false,
  emptyText: empty,
}: {
  queryString: string;
  version: number;
  onOpen: (id: string) => void;
  showOwner?: boolean;
  emptyText: string;
}) {
  const [chats, setChats] = useState<AdminChat[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let stale = false;
    getGatedJson<{ chats: AdminChat[] }>(`/api/admin/chats?${queryString}`, { label: tr("Chats") })
      .then((data) => {
        if (stale || !data) return;
        setChats(data.chats);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!stale) setError(errorMessage(err, tr("Couldn’t load chats.")));
      });
    return () => {
      stale = true;
    };
  }, [queryString, version]);
  if (error) return <NoticeBanner tone="danger" className="my-2">{error}</NoticeBanner>;
  if (chats === null) return <p className="px-1.5 py-3 text-sm text-[var(--color-text-muted)]">{tr("Loading chats…")}</p>;
  if (chats.length === 0) {
    return showOwner ? <ConnEmpty>{empty}</ConnEmpty> : <p className="px-1.5 py-3 text-[0.8rem] text-[var(--color-text-muted)]">{empty}</p>;
  }
  return (
    <ul className={cn("m-0 grid list-none gap-0 p-0", showOwner && "rounded-md border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2")} data-testid="admin-chats-list">
      {chats.map((c) => (
        <li key={c.id} className="border-b border-[var(--color-border-subtle)] last:border-b-0">
          <button
            type="button"
            onClick={() => onOpen(c.id)}
            className="grid w-full grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1 rounded-[var(--radius-sm)] px-1.5 py-2.5 text-left hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            data-testid="admin-chat-row"
          >
            <span className="grid min-w-0 gap-1">
              <span className="truncate text-[0.875rem] font-medium text-[var(--color-text-primary)]">{c.title || tr("Untitled chat")}</span>
              <span className="flex flex-wrap items-center gap-1.5">
                <span className="min-w-0 truncate text-[0.75rem] text-[var(--color-text-muted)]">
                  {showOwner ? tr("{0} · {1}", [c.owner_email, formatAgo(c.updated_at)]) : formatAgo(c.updated_at)}
                </span>
                <ChatBadges chat={c} />
              </span>
            </span>
            <Icon name="chevron-right" className="size-4 shrink-0 text-[var(--color-text-muted)]" />
          </button>
        </li>
      ))}
    </ul>
  );
}
