"use client";

// Settings → Admin → Users (fleet-unified settings pass): the admin surface
// for the RBAC layer (#237) merged with the old /admin usage table. One table
// joins GET /api/admin/users (provisioned accounts: role/team/ops flag) with
// GET /api/admin/stats (per-email usage) keyed by email, and lets an admin
// manage accounts end-to-end — create, reassign role/team, reset passwords,
// delete — so user management no longer requires CLI access to the box
// (`fleet admin add` / `fleet chat user …` remain the scriptable equivalents).
// Roles gate access on the chat server (viewer = read-only, admin = full +
// these pages); granting admin also grants the Operations Center admin row
// server-side, surfaced here via the ops badge. Row edits live in the
// kebab-opened popover (the design's .user-pop).
//
// Gating: useIsAdmin is VISIBILITY only (members are bounced back to
// /settings); authorization stays server-side — every endpoint here
// independently 403s non-admins, and fetchUsers surfaces that below.

import { useEffect, useState, type MouseEvent } from "react";
import { useRouter } from "next/navigation";

import { fetchStats, formatAgo, formatUSD, type UserStat } from "../lib";
import {
  btnClass,
  InlineConfirmButton,
  RevealButton,
  Segmented,
  SETTINGS_INPUT,
} from "../../ui/atoms";
import { ConnField, ConnForm, ConnPanel, SetSection } from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

export type AdminUser = {
  email: string;
  role: string;
  team_id: string;
  created_at: number;
  updated_at: number;
  ops_center_admin?: boolean;
  // Operations Center (sched-plane) role: "admin" | "client" | "readonly" |
  // "" (no ops access). Independent of the chat role.
  ops_center_role?: string;
};

const ROLES = ["member", "viewer", "admin"] as const;
type Role = (typeof ROLES)[number];

const ROLE_OPTIONS = [
  { value: "member", label: "Member" },
  { value: "viewer", label: "Viewer" },
  { value: "admin", label: "Admin" },
] as const satisfies readonly { value: Role; label: string }[];

// Operations Center roles (the sched plane). "client" is presented as
// "Operator" — it can create and run tasks; "readonly" watches.
const OPS_ROLES = ["none", "readonly", "client", "admin"] as const;
type OpsRole = (typeof OPS_ROLES)[number];
const OPS_ROLE_OPTIONS = [
  { value: "none", label: "None" },
  { value: "readonly", label: "Viewer" },
  { value: "client", label: "Operator" },
  { value: "admin", label: "Admin" },
] as const satisfies readonly { value: OpsRole; label: string }[];
const opsRoleOf = (u: AdminUser): OpsRole =>
  (OPS_ROLES as readonly string[]).includes(u.ops_center_role ?? "")
    ? ((u.ops_center_role || "none") as OpsRole)
    : u.ops_center_admin
      ? "admin"
      : "none";

// generatePassword returns a random 16-char password from an unambiguous
// alphabet (no 0/O/1/l/I). crypto.getRandomValues + rejection sampling keeps
// the distribution uniform; 16 chars over 55 symbols ≈ 92 bits.
function generatePassword(): string {
  const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
  const out: string[] = [];
  const buf = new Uint8Array(32);
  while (out.length < 16) {
    crypto.getRandomValues(buf);
    for (const b of buf) {
      if (out.length >= 16) break;
      if (b < alphabet.length * Math.floor(256 / alphabet.length)) {
        out.push(alphabet[b % alphabet.length]);
      }
    }
  }
  return out.join("");
}

async function fetchUsers(): Promise<AdminUser[] | null> {
  const response = await fetch("/api/admin/users", { cache: "no-store" });
  if (response.status === 401) {
    window.location.href = "/login";
    return null;
  }
  if (response.status === 403) {
    throw new Error("You are not an admin.");
  }
  if (!response.ok) {
    throw new Error(`Users request failed: ${response.status}`);
  }
  const data = (await response.json()) as { users?: AdminUser[] };
  return data.users ?? [];
}

async function readErrorText(res: Response, fallback: string): Promise<string> {
  const text = (await res.text()).trim();
  return text.length > 0 && text.length <= 200 ? text : fallback;
}

// One table row: the union of a provisioned account and its usage stats,
// keyed by email. Either side may be missing — a provisioned user with no
// activity yet shows "—" in the numeric cells; an email that only appears in
// the stats (activity from before account provisioning existed) renders
// read-only, with nothing to edit.
type Row = { email: string; account: AdminUser | null; stat: UserStat | null };

function joinRows(users: AdminUser[] | null, stats: UserStat[] | null): Row[] {
  const byEmail = new Map<string, Row>();
  for (const u of users ?? []) {
    byEmail.set(u.email, { email: u.email, account: u, stat: null });
  }
  for (const s of stats ?? []) {
    const existing = byEmail.get(s.email);
    if (existing) {
      existing.stat = s;
    } else {
      byEmail.set(s.email, { email: s.email, account: null, stat: s });
    }
  }
  return [...byEmail.values()].sort((a, b) => a.email.localeCompare(b.email));
}

// Design table metrics (.users-table on the base table styles): th/td
// 0.55rem/0.5rem padding, subtle bottom rule dropped on the last row, numeric
// columns right-aligned tabular-nums.
const TD =
  "whitespace-nowrap border-b border-[var(--color-border-subtle)] px-2 py-[0.55rem] text-left group-last/row:border-b-0";
const TD_NUM = `${TD} text-right [font-variant-numeric:tabular-nums]`;
const TH =
  "whitespace-nowrap border-b border-[var(--color-border-subtle)] px-2 py-[0.55rem] text-left text-[0.7rem] font-semibold uppercase text-[var(--color-text-muted)]";
const TH_NUM = `${TH} text-right`;

// The design's .admin-badge (accent) and its neutral sibling for Viewer/ops.
function RoleBadge({
  accent = false,
  title,
  children,
}: {
  accent?: boolean;
  title?: string;
  children: string;
}) {
  return (
    <span
      title={title}
      className={[
        "inline-block rounded-[0.35rem] px-[0.4rem] py-[0.12rem] text-[0.56rem] font-bold uppercase tracking-[0.08em]",
        accent
          ? "bg-[color-mix(in_srgb,var(--color-accent)_14%,transparent)] text-[var(--color-accent)]"
          : "bg-[var(--color-overlay-soft)] text-[var(--color-text-muted)]",
      ].join(" ")}
    >
      {children}
    </span>
  );
}

// The kebab popover's state: which row it edits, its fixed-position anchor,
// and the pending (unsaved) role/team edits.
type Menu = {
  email: string;
  x: number;
  y: number;
  role: Role;
  team: string;
  opsRole: OpsRole;
};

const MENU_WIDTH_PX = 248; // 15.5rem
const MENU_EST_HEIGHT_PX = 250; // flip-above threshold, per the design

export default function AdminUsersPage() {
  const router = useRouter();
  const admin = useIsAdmin();

  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [stats, setStats] = useState<UserStat[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Per-email status for inline feedback ("saving" | "saved" | error text).
  const [rowStatus, setRowStatus] = useState<Record<string, string>>({});
  // Per-email freshly reset password, shown ONCE until dismissed/navigated.
  const [resetShown, setResetShown] = useState<Record<string, string>>({});
  const [menu, setMenu] = useState<Menu | null>(null);
  // ── discovery toolbar state (search / filters / grouping) ──
  const [query, setQuery] = useState("");
  const [filterRole, setFilterRole] = useState("all");
  const [filterOps, setFilterOps] = useState("all");
  const [filterTeam, setFilterTeam] = useState("all");

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  useEffect(() => {
    if (admin !== "admin") return;
    let stale = false;
    fetchUsers()
      .then((rows) => {
        if (stale || rows === null) return;
        setUsers(rows);
      })
      .catch((err: unknown) => {
        if (stale) return;
        setError(err instanceof Error ? err.message : "Failed to load.");
      })
      .finally(() => {
        if (stale) return;
        setLoading(false);
      });
    // Usage stats are enrichment on this page — a failure just leaves the
    // numeric cells at "—" (the users fetch above surfaces the real
    // auth/availability error; a 401 already redirected inside fetchStats).
    fetchStats()
      .then((rows) => {
        if (stale || rows === null) return;
        setStats(rows);
      })
      .catch(() => {});
    return () => {
      stale = true;
    };
  }, [admin]);

  // The popover closes on any outside click or Escape (the design's
  // document-level listeners); clicks inside stop propagation below.
  useEffect(() => {
    if (!menu) return;
    const close = () => setMenu(null);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenu(null);
    };
    document.addEventListener("click", close);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("click", close);
      document.removeEventListener("keydown", onKey);
    };
  }, [menu]);

  const openMenu = (u: AdminUser, e: MouseEvent<HTMLButtonElement>) => {
    e.stopPropagation();
    // Fixed positioning, ported from the design's openMenu math: clamp x into
    // the viewport, open below the kebab, flip above when it would overflow.
    const rect = e.currentTarget.getBoundingClientRect();
    const x = Math.max(
      8,
      Math.min(
        rect.right - MENU_WIDTH_PX,
        window.innerWidth - MENU_WIDTH_PX - 8,
      ),
    );
    let y = rect.bottom + 6;
    if (y + MENU_EST_HEIGHT_PX > window.innerHeight)
      y = Math.max(8, rect.top - MENU_EST_HEIGHT_PX);
    setMenu({
      email: u.email,
      x,
      y,
      role: (ROLES as readonly string[]).includes(u.role)
        ? (u.role as Role)
        : "member",
      team: u.team_id,
      opsRole: opsRoleOf(u),
    });
  };

  const save = async (
    u: AdminUser,
    next: { role: string; team_id: string; ops_role?: string },
  ) => {
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      // ops_role rides along only when it actually changed: an explicit write
      // pins an ops-plane row, and untouched accounts should keep following
      // the implied chat-admin semantics.
      const body: Record<string, string> = {
        role: next.role,
        team_id: next.team_id,
      };
      if (next.ops_role !== undefined && next.ops_role !== opsRoleOf(u)) {
        body.ops_role = next.ops_role;
      }
      const res = await fetch(
        `/api/admin/users/${encodeURIComponent(u.email)}`,
        {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
      );
      if (!res.ok) {
        throw new Error(
          await readErrorText(res, `Save failed (${res.status}).`),
        );
      }
      const updated = (await res.json()) as AdminUser;
      setUsers((prev) =>
        prev ? prev.map((x) => (x.email === u.email ? updated : x)) : prev,
      );
      setRowStatus((s) => ({ ...s, [u.email]: "saved" }));
    } catch (err) {
      setRowStatus((s) => ({
        ...s,
        [u.email]: err instanceof Error ? err.message : "Save failed.",
      }));
    }
  };

  const remove = async (u: AdminUser) => {
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      const res = await fetch(
        `/api/admin/users/${encodeURIComponent(u.email)}`,
        { method: "DELETE" },
      );
      if (!res.ok) {
        throw new Error(
          await readErrorText(res, `Delete failed (${res.status}).`),
        );
      }
      setUsers((prev) =>
        prev ? prev.filter((x) => x.email !== u.email) : prev,
      );
    } catch (err) {
      setRowStatus((s) => ({
        ...s,
        [u.email]: err instanceof Error ? err.message : "Delete failed.",
      }));
    }
  };

  const resetPassword = async (u: AdminUser) => {
    const password = generatePassword();
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      const res = await fetch(
        `/api/admin/users/${encodeURIComponent(u.email)}/password`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ password }),
        },
      );
      if (!res.ok) {
        throw new Error(
          await readErrorText(res, `Reset failed (${res.status}).`),
        );
      }
      setRowStatus((s) => ({ ...s, [u.email]: "saved" }));
      setResetShown((s) => ({ ...s, [u.email]: password }));
    } catch (err) {
      setRowStatus((s) => ({
        ...s,
        [u.email]: err instanceof Error ? err.message : "Reset failed.",
      }));
    }
  };

  // ── add-user form state ──
  const [addOpen, setAddOpen] = useState(false);
  const [newEmail, setNewEmail] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [newRole, setNewRole] = useState<Role>("member");
  const [addStatus, setAddStatus] = useState<string | null>(null);
  const [addedPassword, setAddedPassword] = useState<{
    email: string;
    password: string;
  } | null>(null);

  const addUser = async () => {
    setAddStatus("saving");
    setAddedPassword(null);
    try {
      const res = await fetch("/api/admin/users", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          email: newEmail.trim(),
          password: newPassword,
          role: newRole,
        }),
      });
      if (!res.ok) {
        throw new Error(
          await readErrorText(res, `Create failed (${res.status}).`),
        );
      }
      const created = (await res.json()) as AdminUser;
      setUsers((prev) =>
        prev
          ? [...prev, created].sort((a, b) => a.email.localeCompare(b.email))
          : [created],
      );
      setAddedPassword({ email: created.email, password: newPassword });
      setNewEmail("");
      setNewPassword("");
      setNewRole("member");
      setAddStatus(null);
      setAddOpen(false);
    } catch (err) {
      setAddStatus(err instanceof Error ? err.message : "Create failed.");
    }
  };

  const addDisabled =
    addStatus === "saving" || newEmail.trim() === "" || newPassword.length < 8;

  if (admin !== "admin") return null;

  const allRows = joinRows(users, stats);
  const teams = Array.from(
    new Set(
      allRows.map((r) => r.account?.team_id.trim() ?? "").filter(Boolean),
    ),
  ).sort();
  const q = query.trim().toLowerCase();
  const rows = allRows.filter((r) => {
    const a = r.account;
    if (q) {
      const hay = `${r.email} ${a?.team_id ?? ""}`.toLowerCase();
      if (!hay.includes(q)) return false;
    }
    if (filterRole !== "all" && (a?.role ?? "member") !== filterRole)
      return false;
    if (filterOps !== "all" && (a ? opsRoleOf(a) : "none") !== filterOps)
      return false;
    if (filterTeam !== "all") {
      const team = a?.team_id.trim() ?? "";
      if (filterTeam === "(none)" ? team !== "" : team !== filterTeam)
        return false;
    }
    return true;
  });
  const filtersActive =
    q !== "" ||
    filterRole !== "all" ||
    filterOps !== "all" ||
    filterTeam !== "all";
  const menuAccount = menu
    ? (users?.find((u) => u.email === menu.email) ?? null)
    : null;
  const menuDirty =
    menu && menuAccount
      ? menu.role !== menuAccount.role ||
        menu.team !== menuAccount.team_id ||
        menu.opsRole !== opsRoleOf(menuAccount)
      : false;

  return (
    <SetSection
      title="Users"
      intro={
        <>
          View usage and manage roles of everyone in the workspace. Numbers here
          are all-time interactive-chat activity per account; for windowed spend
          across chat + scheduled tasks and the per-user adoption audit, open
          the <a href="/orchestrator?tab=usage">Operations Center → Usage</a>{" "}
          and <a href="/orchestrator?tab=adoption">Adoption</a> views.
        </>
      }
    >
      {error ? (
        <NoticeBanner tone="danger">{error}</NoticeBanner>
      ) : (
        <ConnPanel>
          {/* ── discovery toolbar: search, filters, grouping ── */}
          <div className="mb-3 flex flex-wrap items-center gap-[0.55rem]">
            <input
              aria-label="Search users"
              placeholder="Search email, team, tag…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className={`${SETTINGS_INPUT} min-w-[12rem] flex-1 basis-[14rem]`}
            />
            <select
              aria-label="Filter by chat role"
              value={filterRole}
              onChange={(e) => setFilterRole(e.target.value)}
              className={`${SETTINGS_INPUT} w-auto`}
            >
              <option value="all">Chat: all</option>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  Chat: {r}
                </option>
              ))}
            </select>
            <select
              aria-label="Filter by Ops Center role"
              value={filterOps}
              onChange={(e) => setFilterOps(e.target.value)}
              className={`${SETTINGS_INPUT} w-auto`}
            >
              <option value="all">Ops: all</option>
              {OPS_ROLE_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  Ops: {o.label.toLowerCase()}
                </option>
              ))}
            </select>
            <select
              aria-label="Filter by team"
              value={filterTeam}
              onChange={(e) => setFilterTeam(e.target.value)}
              className={`${SETTINGS_INPUT} w-auto`}
            >
              <option value="all">Team: all</option>
              <option value="(none)">Team: none</option>
              {teams.map((t) => (
                <option key={t} value={t}>
                  Team: {t}
                </option>
              ))}
            </select>
          </div>
          <p className="mb-2 text-[0.72rem] text-[var(--color-text-muted)]">
            {rows.length}
            {filtersActive ? ` of ${allRows.length}` : ""} account
            {rows.length === 1 ? "" : "s"}
            {" · "}
            {allRows.filter((r) => r.account?.role === "admin").length} admin
            {" · "}
            {teams.length} team{teams.length === 1 ? "" : "s"}
          </p>

          <div className="overflow-x-auto [scrollbar-width:thin]">
            <table className="w-full border-collapse text-[0.85rem] text-[var(--color-text-secondary)]">
              <thead>
                <tr>
                  <th className={TH}>User</th>
                  <th className={TH_NUM}>Convs</th>
                  <th className={TH_NUM}>Pinned</th>
                  <th className={TH_NUM}>Turns</th>
                  <th
                    className={TH_NUM}
                    title="All-time interactive chat spend for this account. Scheduled-task spend and windowed reporting live in Operations Center → Usage."
                  >
                    Chat spend
                  </th>
                  <th className={`${TH} w-[2.4rem] text-right`} />
                </tr>
              </thead>
              <tbody>
                {loading ? (
                  <tr className="group/row">
                    <td
                      colSpan={6}
                      className={`${TD} py-4 text-center text-[var(--color-text-muted)]`}
                    >
                      Loading…
                    </td>
                  </tr>
                ) : rows.length > 0 ? (
                  rows.map((row) => {
                    // Consts (not row.account/.stat property accesses) so the
                    // null-narrowing flows into the JSX callbacks below.
                    const { account, stat } = row;
                    const status = rowStatus[row.email];
                    return (
                      <tr key={row.email} className="group/row">
                        <td className={`${TD} min-w-[12rem] whitespace-normal`}>
                          <span className="flex flex-wrap items-center gap-[0.45rem]">
                            <span className="text-[0.8rem] text-[var(--color-text-primary)]">
                              {row.email}
                            </span>
                            {account?.role === "admin" ? (
                              <RoleBadge accent>Admin</RoleBadge>
                            ) : null}
                            {account?.role === "viewer" ? (
                              <RoleBadge>Viewer</RoleBadge>
                            ) : null}
                            {account && opsRoleOf(account) !== "none" ? (
                              <RoleBadge
                                title={`Operations Center ${
                                  {
                                    admin: "admin",
                                    client: "operator (creates tasks)",
                                    readonly: "viewer (read-only)",
                                  }[
                                    opsRoleOf(account) as
                                      "admin" | "client" | "readonly"
                                  ]
                                }`}
                              >
                                {`ops: ${
                                  {
                                    admin: "admin",
                                    client: "operator",
                                    readonly: "viewer",
                                  }[
                                    opsRoleOf(account) as
                                      "admin" | "client" | "readonly"
                                  ]
                                }`}
                              </RoleBadge>
                            ) : null}
                          </span>
                          <span className="mt-[0.18rem] block text-[0.7rem] text-[var(--color-text-muted)]">
                            last active {formatAgo(stat?.last_activity ?? 0)}
                            {account?.team_id.trim()
                              ? ` · team: ${account.team_id.trim()}`
                              : ""}
                          </span>
                          {resetShown[row.email] ? (
                            <span className="mt-[0.18rem] block text-[0.7rem] text-[var(--color-text-muted)]">
                              New password (shown once):{" "}
                              <code className="font-[family-name:var(--font-code)] text-[var(--color-text-secondary)]">
                                {resetShown[row.email]}
                              </code>
                            </span>
                          ) : null}
                          {status ? (
                            <span
                              className={`mt-[0.18rem] block text-[0.75rem] ${
                                status === "saving" || status === "saved"
                                  ? "text-[var(--color-text-muted)]"
                                  : "text-[var(--color-danger-soft)]"
                              }`}
                              title={status}
                            >
                              {status === "saving"
                                ? "Saving…"
                                : status === "saved"
                                  ? "Saved"
                                  : status}
                            </span>
                          ) : null}
                        </td>
                        <td className={TD_NUM}>
                          {stat ? stat.conversation_count : "—"}
                        </td>
                        <td className={TD_NUM}>
                          {stat ? stat.pinned_count : "—"}
                        </td>
                        <td className={TD_NUM}>
                          {stat ? stat.total_turns : "—"}
                        </td>
                        <td className={TD_NUM}>
                          {stat ? formatUSD(stat.total_cost_usd) : "—"}
                        </td>
                        <td className={`${TD} w-[2.4rem] text-right`}>
                          {account ? (
                            <button
                              type="button"
                              aria-label={`Edit ${row.email}`}
                              aria-haspopup="true"
                              aria-expanded={menu?.email === row.email}
                              onClick={(e) => openMenu(account, e)}
                              className="inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                            >
                              <Icon name="dots" className="size-4" />
                            </button>
                          ) : null}
                        </td>
                      </tr>
                    );
                  })
                ) : (
                  <tr className="group/row">
                    <td
                      colSpan={6}
                      className={`${TD} py-4 text-center text-[var(--color-text-muted)]`}
                    >
                      No users provisioned yet — add one below.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* ── row-edit popover (the design's .user-pop) ── */}
          {menu && menuAccount ? (
            <div
              style={{ left: menu.x, top: menu.y }}
              onClick={(e) => e.stopPropagation()}
              className="fixed z-[400] grid w-[15.5rem] gap-[0.7rem] rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] px-[0.85rem] py-[0.8rem] shadow-[var(--shadow-md)] motion-safe:animate-set-fade"
            >
              <div className="text-[0.76rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                {menu.email}
              </div>
              <div className="flex items-center justify-between gap-[0.6rem]">
                <span
                  className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]"
                  title="Chat permissions: what this account can do in chat. Viewer is read-only; Admin includes these settings pages."
                >
                  Chat
                </span>
                <Segmented
                  value={menu.role}
                  options={ROLE_OPTIONS}
                  onChange={(role) => setMenu({ ...menu, role })}
                  label="Chat permissions"
                />
              </div>
              <div className="flex items-center justify-between gap-[0.6rem]">
                <span
                  className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]"
                  title="Ops Center permissions: Viewer sees tasks and logs; Operator also creates tasks; Admin controls the ops plane. Chat Admin implies Ops Admin."
                >
                  Ops Center
                </span>
                <Segmented
                  value={menu.opsRole}
                  options={OPS_ROLE_OPTIONS}
                  onChange={(opsRole) => setMenu({ ...menu, opsRole })}
                  label="Ops Center permissions"
                />
              </div>
              <div className="flex items-center justify-between gap-[0.6rem]">
                <span className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]">
                  Team
                </span>
                <input
                  aria-label={`Team for ${menu.email}`}
                  value={menu.team}
                  placeholder="—"
                  onChange={(e) => setMenu({ ...menu, team: e.target.value })}
                  className={`${SETTINGS_INPUT} min-h-[2.1rem]! min-w-0 flex-1 px-[0.55rem]! py-[0.3rem]! text-[0.78rem]!`}
                />
              </div>
              <div className="mt-[0.05rem] flex justify-end gap-[0.45rem]">
                <button
                  type="button"
                  onClick={() => setMenu(null)}
                  className={btnClass({ sm: true, reveal: true })}
                >
                  Cancel
                </button>
                <button
                  type="button"
                  disabled={!menuDirty}
                  onClick={() => {
                    setMenu(null);
                    void save(menuAccount, {
                      role: menu.role,
                      team_id: menu.team,
                      ops_role: menu.opsRole,
                    });
                  }}
                  className={btnClass({ variant: "primary", sm: true })}
                >
                  Save
                </button>
              </div>
              <div className="h-px bg-[var(--color-border)]" />
              <div className="flex flex-wrap items-center justify-between gap-[0.45rem]">
                <button
                  type="button"
                  onClick={() => {
                    setMenu(null);
                    void resetPassword(menuAccount);
                  }}
                  className={btnClass({ sm: true, reveal: true })}
                >
                  Reset password
                </button>
                <InlineConfirmButton
                  label="Delete user"
                  onConfirm={() => {
                    setMenu(null);
                    void remove(menuAccount);
                  }}
                />
              </div>
            </div>
          ) : null}

          {/* ── add user ── */}
          <div className="mt-2 border-t border-[var(--color-border-subtle)] pt-3">
            <RevealButton
              open={addOpen}
              closedLabel="Add user"
              onClick={() => setAddOpen((o) => !o)}
            />
            {addOpen ? (
              <ConnForm className="mt-[0.7rem]">
                <ConnField label="Email" grow>
                  <input
                    aria-label="New user email"
                    type="email"
                    value={newEmail}
                    placeholder="email@example.com"
                    onChange={(e) => setNewEmail(e.target.value)}
                    className={SETTINGS_INPUT}
                  />
                </ConnField>
                <ConnField label="Password" grow>
                  <div className="flex gap-[0.45rem]">
                    <input
                      aria-label="New user password"
                      type="text"
                      value={newPassword}
                      placeholder="min 8 chars"
                      onChange={(e) => setNewPassword(e.target.value)}
                      className={`${SETTINGS_INPUT} font-[family-name:var(--font-code)] text-[0.85rem]!`}
                    />
                    <button
                      type="button"
                      onClick={() => setNewPassword(generatePassword())}
                      className={btnClass({ sm: true })}
                    >
                      Generate
                    </button>
                  </div>
                </ConnField>
                <ConnField label="Role">
                  {/* .select-wrap: hide the native chevron and draw the design's. */}
                  <span className="relative block after:pointer-events-none after:absolute after:right-[0.7rem] after:top-1/2 after:size-2 after:-translate-y-[65%] after:rotate-45 after:border-b-[1.5px] after:border-r-[1.5px] after:border-[var(--color-text-muted)] after:content-['']">
                    <select
                      aria-label="New user role"
                      value={newRole}
                      onChange={(e) => setNewRole(e.target.value as Role)}
                      className={`${SETTINGS_INPUT} appearance-none pr-8!`}
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                  </span>
                </ConnField>
                <button
                  type="button"
                  onClick={() => void addUser()}
                  disabled={addDisabled}
                  className={btnClass({ variant: "primary" })}
                >
                  {addStatus === "saving" ? "Adding…" : "Add user"}
                </button>
              </ConnForm>
            ) : null}
            {addStatus && addStatus !== "saving" ? (
              <p className="mt-2 text-[0.75rem] text-[var(--color-danger-soft)]">
                {addStatus}
              </p>
            ) : null}
            {addedPassword ? (
              <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
                Created{" "}
                <span className="text-[var(--color-text-secondary)]">
                  {addedPassword.email}
                </span>{" "}
                — password (shown once):{" "}
                <code className="font-[family-name:var(--font-code)] text-[var(--color-text-secondary)]">
                  {addedPassword.password}
                </code>
              </p>
            ) : null}
            <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
              Chat and Ops Center permissions are separate: chat roles gate this
              app, Ops Center roles gate the task scheduler (Viewer watches,
              Operator creates tasks). Granting chat{" "}
              <span className="uppercase">admin</span> also grants Ops Center
              admin; explicit Ops grants survive chat-role changes. CLI
              equivalent for the admin case:{" "}
              <code className="font-[family-name:var(--font-code)]">
                fleet admin add
              </code>
              .
            </p>
          </div>
        </ConnPanel>
      )}
    </SetSection>
  );
}
