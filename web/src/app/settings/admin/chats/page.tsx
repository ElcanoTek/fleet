import { t as tr } from "@/app/lib/i18n";
// Settings → Admin → Chats — chat administration (ADR-0115). An administrator
// finds any chat on the box, reads it, sends a message into it (the turn runs
// as the chat's owner, labelled as the administrator's), stops it, transfers
// it, changes its title / approval mode / archive state, writes a notice into
// it, and edits, pauses or resumes its patrols. Every change takes a reason,
// is recorded (GET /api/admin/chat-actions) and is announced in the chat.
// `fleet chats` on the box does the same from a terminal.

import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useRouter } from "@/app/lib/navigation";
import { responseErrorText } from "@/app/lib/responseError";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { cn } from "@/lib/utils";

import { AdminGateFallback } from "../../AdminGateFallback";
import { getGatedJson } from "../../lib/getGated";
import { ConnBadge, SETTINGS_INPUT, btnClass } from "../../ui/atoms";
import { ConnEmpty, ConnGroup, ConnPanel, ConnPanelHead, ConnPanelSub, SetSection } from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { formatAgo } from "../lib";

export type AdminChat = {
  id: string;
  title: string;
  owner_email: string;
  model: string;
  approval_mode: string;
  persona: string;
  lockdown: boolean;
  archived: boolean;
  multiplayer: boolean;
  participants?: string[];
  team_shared: boolean;
  created_at: number;
  updated_at: number;
  active_patrols: number;
  pending_approvals: number;
  queued_inputs: number;
  turn_running: boolean;
};

export type AdminPatrol = {
  id: string;
  title: string;
  prompt: string;
  prompt_sha256: string;
  cron?: string;
  timezone: string;
  status: string;
  next_run_at: number;
  user_email: string;
  fire_in_flight: boolean;
};

type AdminAction = {
  id: number;
  created_at: number;
  actor: string;
  via: string;
  action: string;
  reason?: string;
  detail?: string;
};

type TranscriptEntry = {
  id: number;
  created_at: number;
  role: string;
  type: string;
  kind?: string;
  sender?: string;
  tool?: string;
  text: string;
  is_error?: boolean;
  truncated?: boolean;
};

type AdminRequest = {
  id: string;
  kind: string;
  state: string;
  result?: string;
  input_state?: string;
};

type Detail = {
  chat: AdminChat;
  patrols: AdminPatrol[];
  actions: AdminAction[];
};

// sendJSON is the page's one mutation path: a JSON body, the server's reason
// as the error on failure, the JSON answer (or null for a 204) on success.
async function sendJSON<T>(url: string, method: string, body: unknown): Promise<T | null> {
  const res = await fetch(url, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw new Error(await responseErrorText(res, tr("The change failed")));
  if (res.status === 204) return null;
  return (await res.json()) as T;
}

/** lineDiff is a small unified-style line diff for the patrol editor's preview. */
export function lineDiff(a: string, b: string): { kind: " " | "-" | "+"; text: string }[] {
  const al = a.split("\n");
  const bl = b.split("\n");
  if ((al.length + 1) * (bl.length + 1) > 4_000_000) return [];
  const n = al.length;
  const m = bl.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i][j] = al[i] === bl[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out: { kind: " " | "-" | "+"; text: string }[] = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && al[i] === bl[j]) {
      out.push({ kind: " ", text: al[i] });
      i++;
      j++;
    } else if (i < n && (j === m || lcs[i + 1][j] >= lcs[i][j + 1])) {
      out.push({ kind: "-", text: al[i] });
      i++;
    } else {
      out.push({ kind: "+", text: bl[j] });
      j++;
    }
  }
  return out;
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="grid gap-1 text-[0.78rem] font-medium text-[var(--color-text-secondary)]">
      {label}
      {children}
    </label>
  );
}

function Status({ text, tone }: { text: string | null; tone: "ok" | "error" }) {
  if (!text) return null;
  return (
    <p
      role={tone === "error" ? "alert" : "status"}
      className={cn("m-0 text-[0.75rem]", tone === "error" ? "text-[var(--color-danger)]" : "text-[var(--color-text-muted)]")}
    >
      {text}
    </p>
  );
}

function entryLabel(e: TranscriptEntry, owner: string): string {
  if (e.type === "notice") return tr("Notice");
  if (e.type === "tool_call") return tr("Tool call · {0}", [e.tool ?? ""]);
  if (e.type === "tool_result") return e.is_error ? tr("Tool error · {0}", [e.tool ?? ""]) : tr("Tool result · {0}", [e.tool ?? ""]);
  if (e.role === "user" && e.kind === "admin_message") return tr("Administrator · {0}", [e.sender ?? ""]);
  if (e.role === "user" && e.kind === "chat_patrol") return tr("Patrol run");
  if (e.role === "user" && e.kind) return tr("Fleet");
  if (e.role === "user") return e.sender || owner;
  return tr("Assistant");
}

export default function AdminChatsPage() {
  const router = useRouter();
  const admin = useIsAdmin();
  const [query, setQuery] = useState("");
  const [owner, setOwner] = useState("");
  const [days, setDays] = useState("30");
  const [archived, setArchived] = useState(false);
  const [chats, setChats] = useState<AdminChat[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  const load = useCallback(async () => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (owner.trim()) params.set("owner", owner.trim());
    if (days !== "0") params.set("active_days", days);
    if (archived) params.set("archived", "1");
    params.set("limit", "200");
    try {
      const data = await getGatedJson<{ chats: AdminChat[] }>(`/api/admin/chats?${params.toString()}`, { label: tr("Chats") });
      if (data) {
        setChats(data.chats);
        setListError(null);
      }
    } catch (err) {
      setListError(errorMessage(err, tr("Couldn’t load chats.")));
    }
  }, [query, owner, days, archived]);

  useEffect(() => {
    if (admin !== "admin") return;
    const t = window.setTimeout(() => void load(), 250);
    return () => window.clearTimeout(t);
  }, [admin, load]);

  if (admin !== "admin") return <AdminGateFallback state={admin} />;

  return (
    <SetSection
      title={tr("Chats")}
      intro={tr("Find any chat in this workspace and look after it: read it, send it a message, stop it, hand it to someone else, or fix its patrols. Every change asks for a reason, is recorded, and is noted in the chat itself. A message you send runs as the chat’s owner, with their connections and approval setting, and is labelled as yours.")}
    >
      <ConnGroup>
        <div className="mb-3 grid gap-2 sm:grid-cols-[1fr_1fr_auto_auto] sm:items-end">
          <Field label={tr("Search titles or ids")}>
            <input className={SETTINGS_INPUT} value={query} onChange={(e) => setQuery(e.target.value)} data-testid="admin-chats-search" />
          </Field>
          <Field label={tr("Owner email")}>
            <input className={SETTINGS_INPUT} value={owner} onChange={(e) => setOwner(e.target.value)} />
          </Field>
          <Field label={tr("Active in")}>
            <select className={SETTINGS_INPUT} value={days} onChange={(e) => setDays(e.target.value)}>
              <option value="7">{tr("Last 7 days")}</option>
              <option value="30">{tr("Last 30 days")}</option>
              <option value="90">{tr("Last 90 days")}</option>
              <option value="0">{tr("Any time")}</option>
            </select>
          </Field>
          <label className="flex min-h-[2.5rem] items-center gap-2 text-[0.8rem] text-[var(--color-text-secondary)]">
            <input type="checkbox" checked={archived} onChange={(e) => setArchived(e.target.checked)} />
            {tr("Include archived")}
          </label>
        </div>
        {listError ? <NoticeBanner tone="danger">{listError}</NoticeBanner> : null}
        {chats === null ? (
          listError ? null : <p className="text-sm text-[var(--color-text-muted)]">{tr("Loading chats…")}</p>
        ) : chats.length === 0 ? (
          <ConnEmpty>{tr("No chats match.")}</ConnEmpty>
        ) : (
          <ul className="m-0 grid list-none gap-0 p-0" data-testid="admin-chats-list">
            {chats.map((c) => (
              <li key={c.id} className="border-b border-[var(--color-border)] last:border-b-0">
                <button
                  type="button"
                  onClick={() => setSelected(selected === c.id ? null : c.id)}
                  aria-expanded={selected === c.id}
                  className="flex w-full flex-wrap items-center gap-x-3 gap-y-1 px-1 py-2.5 text-left hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                >
                  <span className="min-w-0 flex-1 truncate text-sm font-medium text-[var(--color-text-primary)]">{c.title || c.id}</span>
                  <span className="text-[0.75rem] text-[var(--color-text-muted)]">{c.owner_email}</span>
                  <span className="text-[0.75rem] text-[var(--color-text-muted)]">{formatAgo(c.updated_at)}</span>
                  <span className="flex flex-wrap gap-1">
                    {c.turn_running ? <ConnBadge variant="success">{tr("running")}</ConnBadge> : null}
                    {c.active_patrols > 0 ? <ConnBadge>{tr("{0} patrol(s)", [c.active_patrols])}</ConnBadge> : null}
                    {c.pending_approvals > 0 ? <ConnBadge variant="warn">{tr("{0} card(s) waiting", [c.pending_approvals])}</ConnBadge> : null}
                    {c.archived ? <ConnBadge>{tr("archived")}</ConnBadge> : null}
                    {c.lockdown ? <ConnBadge variant="overridden">{tr("Lockdown")}</ConnBadge> : null}
                  </span>
                </button>
                {selected === c.id ? <ChatAdminPanel chatId={c.id} onChanged={() => void load()} /> : null}
              </li>
            ))}
          </ul>
        )}
      </ConnGroup>
    </SetSection>
  );
}

function loadChat(chatId: string, withTools: boolean) {
  return Promise.all([
    getGatedJson<Detail>(`/api/admin/chats/${encodeURIComponent(chatId)}`, { label: tr("Chat") }),
    getGatedJson<{ entries: TranscriptEntry[] }>(
      `/api/admin/chats/${encodeURIComponent(chatId)}/history?tail=40${withTools ? "&tools=1" : ""}`,
      { label: tr("Transcript") },
    ),
  ]);
}

function ChatAdminPanel({ chatId, onChanged }: { chatId: string; onChanged: () => void }) {
  const [detail, setDetail] = useState<Detail | null>(null);
  const [entries, setEntries] = useState<TranscriptEntry[] | null>(null);
  const [withTools, setWithTools] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(() => {
    loadChat(chatId, withTools)
      .then(([d, h]) => {
        if (d) setDetail(d);
        if (h) setEntries(h.entries);
        setError(null);
      })
      .catch((err: unknown) => setError(errorMessage(err, tr("Couldn’t load this chat."))));
  }, [chatId, withTools]);

  useEffect(() => {
    let stale = false;
    loadChat(chatId, withTools)
      .then(([d, h]) => {
        if (stale) return;
        if (d) setDetail(d);
        if (h) setEntries(h.entries);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!stale) setError(errorMessage(err, tr("Couldn’t load this chat.")));
      });
    return () => {
      stale = true;
    };
  }, [chatId, withTools]);

  const changed = useCallback(() => {
    reload();
    onChanged();
  }, [reload, onChanged]);

  if (error) return <NoticeBanner tone="danger" className="my-2">{error}</NoticeBanner>;
  if (!detail) return <p className="px-1 py-2 text-sm text-[var(--color-text-muted)]">{tr("Loading…")}</p>;
  const c = detail.chat;
  return (
    <div className="grid gap-3 px-1 pb-4 pt-1" data-testid="admin-chat-panel">
      <p className="m-0 text-[0.78rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
        {tr("{0} · owner {1} · model {2} · approvals {3} · {4} queued", [c.id, c.owner_email, c.model || "—", c.approval_mode, c.queued_inputs])}
      </p>
      <MessagePanel chat={c} onDone={changed} />
      <ChatSettingsPanel chat={c} onDone={changed} />
      {detail.patrols.map((p) => (
        <PatrolPanel key={p.id} patrol={p} onDone={changed} />
      ))}
      <ConnPanel>
        <ConnPanelHead title={tr("Transcript")}>
          <label className="flex items-center gap-2 text-[0.75rem] text-[var(--color-text-secondary)]">
            <input type="checkbox" checked={withTools} onChange={(e) => setWithTools(e.target.checked)} />
            {tr("Show tool calls")}
          </label>
        </ConnPanelHead>
        <ConnPanelSub>{tr("The latest entries, oldest first. Long entries are cut.")}</ConnPanelSub>
        {entries && entries.length > 0 ? (
          <ol className="m-0 grid max-h-[28rem] list-none gap-2 overflow-auto p-0" data-testid="admin-chat-transcript">
            {entries.map((e) => (
              <li key={e.id} className="rounded-md border border-[var(--color-border)] px-3 py-2">
                <div className="mb-1 flex flex-wrap justify-between gap-2 text-[0.7rem] font-semibold uppercase tracking-[0.05em] text-[var(--color-text-muted)]">
                  <span>{entryLabel(e, c.owner_email)}</span>
                  <span>{formatAgo(e.created_at)}</span>
                </div>
                <p className="m-0 whitespace-pre-wrap text-[0.8rem] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">{e.text}</p>
              </li>
            ))}
          </ol>
        ) : (
          <ConnEmpty>{tr("Nothing to show yet.")}</ConnEmpty>
        )}
      </ConnPanel>
      {detail.actions.length > 0 ? (
        <ConnPanel>
          <ConnPanelHead title={tr("Admin changes to this chat")} />
          <ul className="m-0 mt-2 grid list-none gap-1 p-0 text-[0.78rem] text-[var(--color-text-secondary)]">
            {detail.actions.map((a) => (
              <li key={a.id}>
                {tr("{0} · {1} ({2}) · {3}", [formatAgo(a.created_at), a.actor, a.via, a.action])}
                {a.reason ? ` — ${a.reason}` : ""}
              </li>
            ))}
          </ul>
        </ConnPanel>
      ) : null}
    </div>
  );
}

// useRequestFollower polls an admin request until it is settled (and, for a
// message, until its turn has ended), reporting each step.
function useRequestFollower(onDone: () => void) {
  const [status, setStatus] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(() => () => {
    if (timer.current !== null) window.clearTimeout(timer.current);
  }, []);
  const follow = useCallback(
    (first: AdminRequest) => {
      const started = Date.now();
      const describe = (r: AdminRequest): [string, boolean] => {
        if (r.state === "failed") return [tr("Failed: {0}", [r.result ?? ""]), true];
        if (r.state === "pending") return [tr("Waiting for the server…"), false];
        if (r.state === "running") return [tr("The server is carrying it out…"), false];
        if (r.kind !== "message") return [tr("Done: {0}", [r.result ?? ""]), true];
        if (r.input_state === "queued") return [tr("Queued in the chat (waits for any running turn)…"), false];
        if (r.input_state === "running") return [tr("The turn is running…"), false];
        return [tr("The turn ended ({0}).", [r.input_state || tr("removed")]), true];
      };
      const step = function step(req: AdminRequest): void {
        const [text, done] = describe(req);
        setStatus(text);
        setFailed(req.state === "failed");
        if (done) {
          onDone();
          return;
        }
        if (Date.now() - started > 10 * 60 * 1000) {
          setStatus(tr("Still going after 10 minutes; check the chat later."));
          return;
        }
        timer.current = window.setTimeout(() => {
          fetch(`/api/admin/chat-requests/${encodeURIComponent(req.id)}`, { cache: "no-store" })
            .then(async (res) => {
              if (!res.ok) throw new Error(await responseErrorText(res, tr("Couldn’t check the request")));
              const body = (await res.json()) as { request: AdminRequest };
              step(body.request);
            })
            .catch((err: unknown) => {
              setStatus(errorMessage(err, tr("Couldn’t check the request")));
              setFailed(true);
            });
        }, 2000);
      };
      step(first);
    },
    [onDone],
  );
  return { status, failed, follow, setStatus, setFailed };
}

function MessagePanel({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [message, setMessage] = useState("");
  const [reason, setReason] = useState("");
  const [toEmail, setToEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const { status, failed, follow, setStatus, setFailed } = useRequestFollower(onDone);

  const submit = async (kind: "message" | "stop" | "transfer") => {
    setBusy(true);
    setStatus(null);
    try {
      const body = await sendJSON<{ request: AdminRequest }>(`/api/admin/chats/${encodeURIComponent(chat.id)}/requests`, "POST", {
        kind,
        message: kind === "message" ? message : undefined,
        to_email: kind === "transfer" ? toEmail : undefined,
        reason,
      });
      if (body) follow(body.request);
      if (kind === "message") setMessage("");
    } catch (err) {
      setStatus(errorMessage(err, tr("The change failed")));
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  const onSend = (e: FormEvent) => {
    e.preventDefault();
    void submit("message");
  };
  const canMessage = !chat.archived && !chat.lockdown;
  return (
    <ConnPanel>
      <ConnPanelHead title={tr("Message, stop or transfer")} />
      <ConnPanelSub>
        {tr("A message runs as {0}, with their connections and approval setting; any approval card it raises goes to them. It is shown in the chat as yours.", [chat.owner_email])}
      </ConnPanelSub>
      <form className="grid gap-2" onSubmit={onSend}>
        <Field label={tr("Reason (recorded and shown in the chat)")}>
          <input className={SETTINGS_INPUT} value={reason} onChange={(e) => setReason(e.target.value)} data-testid="admin-chat-reason" />
        </Field>
        <Field label={tr("Message")}>
          <textarea
            className={cn(SETTINGS_INPUT, "min-h-[5rem]")}
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            disabled={!canMessage}
            data-testid="admin-chat-message"
          />
        </Field>
        <div className="flex flex-wrap items-end gap-2">
          <button type="submit" className={btnClass({ variant: "primary", sm: true })} disabled={busy || !canMessage || !message.trim() || !reason.trim()}>
            {tr("Send message")}
          </button>
          <button type="button" className={btnClass({ sm: true, reveal: true, danger: true })} disabled={busy || !reason.trim()} onClick={() => void submit("stop")}>
            {tr("Stop running work")}
          </button>
          <input
            className={cn(SETTINGS_INPUT, "max-w-[16rem]")}
            placeholder={tr("New owner’s email")}
            aria-label={tr("New owner’s email")}
            value={toEmail}
            onChange={(e) => setToEmail(e.target.value)}
          />
          <button type="button" className={btnClass({ sm: true, reveal: true })} disabled={busy || !reason.trim() || !toEmail.includes("@")} onClick={() => void submit("transfer")}>
            {tr("Transfer")}
          </button>
        </div>
        {!canMessage ? <Status tone="ok" text={tr("Archived and Lockdown chats take no admin messages.")} /> : null}
        <Status tone={failed ? "error" : "ok"} text={status} />
      </form>
    </ConnPanel>
  );
}

function ChatSettingsPanel({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [title, setTitle] = useState(chat.title);
  const [model, setModel] = useState(chat.model);
  const [mode, setMode] = useState(chat.approval_mode === "autonomous" ? "" : chat.approval_mode);
  const [archivedState, setArchivedState] = useState(chat.archived);
  const [notice, setNotice] = useState("");
  const [reason, setReason] = useState("");
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);

  const save = async () => {
    const body: Record<string, unknown> = { reason };
    if (title.trim() && title.trim() !== chat.title) body.title = title.trim();
    if (model.trim() && model.trim() !== chat.model) body.model = model.trim();
    if (mode && mode !== chat.approval_mode) body.approval_mode = mode;
    if (archivedState !== chat.archived) body.archived = archivedState;
    if (Object.keys(body).length === 1) {
      setStatus({ text: tr("Nothing to change."), error: false });
      return;
    }
    try {
      await sendJSON(`/api/admin/chats/${encodeURIComponent(chat.id)}`, "PATCH", body);
      setStatus({ text: tr("Saved and noted in the chat."), error: false });
      onDone();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    }
  };
  const postNotice = async () => {
    try {
      await sendJSON(`/api/admin/chats/${encodeURIComponent(chat.id)}/notice`, "POST", { text: notice });
      setNotice("");
      setStatus({ text: tr("Notice posted."), error: false });
      onDone();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    }
  };
  return (
    <ConnPanel>
      <ConnPanelHead title={tr("Chat settings and notices")} />
      <ConnPanelSub>
        {tr("Autonomous approvals stay the owner’s own decision, and a Lockdown chat keeps its model. A notice is a note in the chat that the assistant never reads.")}
      </ConnPanelSub>
      <div className="grid gap-2 sm:grid-cols-2">
        <Field label={tr("Title")}>
          <input className={SETTINGS_INPUT} value={title} onChange={(e) => setTitle(e.target.value)} />
        </Field>
        <Field label={tr("Model")}>
          <input
            className={SETTINGS_INPUT}
            value={model}
            onChange={(e) => setModel(e.target.value)}
            disabled={chat.lockdown}
            title={chat.lockdown ? tr("A Lockdown chat keeps its model.") : undefined}
            data-testid="admin-chat-model"
          />
        </Field>
        <Field label={tr("Approvals")}>
          <select className={SETTINGS_INPUT} value={mode} onChange={(e) => setMode(e.target.value)}>
            {mode === "" ? <option value="">{tr("Autonomous (owner’s choice)")}</option> : null}
            <option value="manual">{tr("Manual")}</option>
            <option value="auto">{tr("Auto-approve")}</option>
          </select>
        </Field>
        <label className="flex items-center gap-2 text-[0.8rem] text-[var(--color-text-secondary)]">
          <input type="checkbox" checked={archivedState} onChange={(e) => setArchivedState(e.target.checked)} />
          {tr("Archived")}
        </label>
        <Field label={tr("Reason (recorded and shown in the chat)")}>
          <input className={SETTINGS_INPUT} value={reason} onChange={(e) => setReason(e.target.value)} />
        </Field>
      </div>
      <div className="mt-2 flex flex-wrap gap-2">
        <button type="button" className={btnClass({ variant: "primary", sm: true })} disabled={!reason.trim()} onClick={() => void save()}>
          {tr("Save settings")}
        </button>
      </div>
      <div className="mt-3 grid gap-2 sm:grid-cols-[1fr_auto] sm:items-end">
        <Field label={tr("Notice")}>
          <input className={SETTINGS_INPUT} value={notice} onChange={(e) => setNotice(e.target.value)} />
        </Field>
        <button type="button" className={btnClass({ sm: true, reveal: true })} disabled={!notice.trim()} onClick={() => void postNotice()}>
          {tr("Post notice")}
        </button>
      </div>
      {status ? <div className="mt-2"><Status tone={status.error ? "error" : "ok"} text={status.text} /></div> : null}
    </ConnPanel>
  );
}

function PatrolPanel({ patrol, onDone }: { patrol: AdminPatrol; onDone: () => void }) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(patrol.prompt);
  const [reason, setReason] = useState("");
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);
  const diff = useMemo(() => (editing ? lineDiff(patrol.prompt, draft.trim()) : []), [editing, patrol.prompt, draft]);
  const changedLines = diff.filter((d) => d.kind !== " ").length;

  const patch = async (body: Record<string, unknown>, ok: string) => {
    try {
      await sendJSON(`/api/admin/patrols/${encodeURIComponent(patrol.id)}`, "PATCH", { ...body, reason });
      setStatus({ text: ok, error: false });
      setEditing(false);
      onDone();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    }
  };
  const live = patrol.status === "active" || patrol.status === "paused";
  return (
    <ConnPanel>
      <ConnPanelHead title={tr("Patrol · {0}", [patrol.title])}>
        <ConnBadge variant={patrol.status === "active" ? "success" : "neutral"}>{patrol.status}</ConnBadge>
      </ConnPanelHead>
      <ConnPanelSub>
        {tr("{0} {1} · owner {2}{3}", [
          patrol.cron ?? "",
          patrol.timezone,
          patrol.user_email,
          patrol.status === "active" && patrol.next_run_at ? tr(" · next run {0}", [new Date(patrol.next_run_at * 1000).toLocaleString()]) : "",
        ])}
      </ConnPanelSub>
      <Field label={tr("Reason (recorded and shown in the chat)")}>
        <input className={SETTINGS_INPUT} value={reason} onChange={(e) => setReason(e.target.value)} />
      </Field>
      <div className="mt-2 flex flex-wrap gap-2">
        {live ? (
          <button
            type="button"
            className={btnClass({ sm: true, reveal: true })}
            disabled={!reason.trim()}
            onClick={() => void patch({ status: patrol.status === "active" ? "paused" : "active" }, patrol.status === "active" ? tr("Patrol paused.") : tr("Patrol resumed."))}
          >
            {patrol.status === "active" ? tr("Pause") : tr("Resume")}
          </button>
        ) : null}
        {live ? (
          <button type="button" className={btnClass({ sm: true, reveal: true })} onClick={() => setEditing((v) => !v)} aria-expanded={editing}>
            {editing ? tr("Cancel editing") : tr("Edit instructions")}
          </button>
        ) : null}
      </div>
      {editing ? (
        <div className="mt-3 grid gap-2">
          <textarea
            className={cn(SETTINGS_INPUT, "min-h-[16rem] font-mono text-[0.75rem]")}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            aria-label={tr("Patrol instructions")}
            data-testid="admin-patrol-prompt"
          />
          {changedLines > 0 ? (
            <pre className="m-0 max-h-72 overflow-auto whitespace-pre-wrap rounded-md border border-[var(--color-border)] p-2 font-mono text-[0.7rem] leading-[1.5] [overflow-wrap:anywhere]" data-testid="admin-patrol-diff">
              {diff
                .map((d, i) => ({ ...d, i }))
                .filter((d, i, all) => d.kind !== " " || all.slice(Math.max(0, i - 2), i + 3).some((x) => x.kind !== " "))
                .map((d) => (
                  <div
                    key={d.i}
                    className={d.kind === "-" ? "text-[var(--color-danger)]" : d.kind === "+" ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]"}
                  >
                    {d.kind} {d.text}
                  </div>
                ))}
            </pre>
          ) : (
            <p className="m-0 text-[0.75rem] text-[var(--color-text-muted)]">{tr("No changes yet.")}</p>
          )}
          <div>
            <button
              type="button"
              className={btnClass({ variant: "primary", sm: true })}
              disabled={changedLines === 0 || !reason.trim() || !draft.trim()}
              onClick={() => void patch({ prompt: draft, expect_sha256: patrol.prompt_sha256 }, tr("Instructions updated and noted in the chat."))}
            >
              {tr("Save instructions")}
            </button>
          </div>
        </div>
      ) : null}
      {status ? <div className="mt-2"><Status tone={status.error ? "error" : "ok"} text={status.text} /></div> : null}
    </ConnPanel>
  );
}
