"use client";

// Settings → Admin → Users (fleet-unified settings pass): the admin surface
// for the RBAC layer (#237) merged with the old /admin usage table. One table
// joins GET /api/admin/users (provisioned accounts: role/team/ops flag) with
// GET /api/admin/stats (per-email usage) keyed by email, and lets an admin
// manage accounts end-to-end — create, reassign role/team, reset passwords,
// delete — so user management no longer requires CLI access to the box
// (`fleet admin add` / `fleet chat user …` remain the scriptable equivalents).
// Roles gate access on the chat server (viewer = read-only) while Operations
// Center roles gate the scheduler. Admin is presented separately because it is
// one unified grant: selecting it grants admin on both planes. Row edits live
// in the kebab-opened popover (the design's .user-pop).
//
// Gating: useIsAdmin is VISIBILITY only (members are bounced back to
// /settings); authorization stays server-side — every endpoint here
// independently 403s non-admins, and fetchUsers surfaces that below.

import { useEffect, useRef, useState, type MouseEvent } from "react";
import Link from "next/link";
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
import { AdminGateFallback } from "../../AdminGateFallback";
import { DeleteRefusal, parseOwnedSharedProjects } from "./DeleteRefusal";
import type { OwnedSharedProject } from "./DeleteRefusal";
import { Icon } from "@/app/shared/ui/Icon";
import { CopyButton } from "@/app/chat/ui/ChatChips";
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
const CHAT_ROLE_LABELS = {
  member: "Contributor",
  viewer: "Viewer",
  admin: "Admin",
} as const satisfies Record<Role, string>;

const CHAT_ROLE_OPTIONS = [
  {
    value: "viewer",
    label: CHAT_ROLE_LABELS.viewer,
    description: "Read-only Chat access: can view but cannot create or change content.",
  },
  {
    value: "member",
    label: CHAT_ROLE_LABELS.member,
    description: "Can actively use Chat, including creating and updating content.",
  },
] as const satisfies readonly {
  value: Role;
  label: string;
  description: string;
}[];

// Operations Center roles (the sched plane). "client" is presented as
// "Contributor" — it can create and run tasks; "readonly" watches.
const OPS_ROLES = ["none", "readonly", "client", "admin"] as const;
type OpsRole = (typeof OPS_ROLES)[number];
const OPS_ROLE_OPTIONS = [
  {
    value: "none",
    label: "None",
    description: "No access to the Ops Center.",
  },
  {
    value: "readonly",
    label: "Viewer",
    description: "Can view Ops Center tasks and logs but cannot change them.",
  },
  {
    value: "client",
    label: "Contributor",
    description: "Can view, create, and run Ops Center tasks.",
  },
] as const satisfies readonly {
  value: OpsRole;
  label: string;
  description: string;
}[];
const ADMIN_OPTIONS = [
  {
    value: "admin",
    label: "Admin",
    description: "Full permissions in both Chat and the Ops Center.",
  },
] as const;
const opsRoleOf = (u: AdminUser): OpsRole =>
  (OPS_ROLES as readonly string[]).includes(u.ops_center_role ?? "")
    ? ((u.ops_center_role || "none") as OpsRole)
    : u.ops_center_admin
      ? "admin"
      : "none";

function PermissionFields({
  role,
  opsRole,
  onChange,
  labelPrefix = "",
}: {
  role: Role;
  opsRole: OpsRole;
  onChange: (next: { role: Role; opsRole: OpsRole }) => void;
  labelPrefix?: string;
}) {
  const ariaPrefix = labelPrefix ? `${labelPrefix} ` : "";
  return (
    <>
      <div className="grid justify-items-start gap-[0.3rem]">
        <span
          className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]"
          title="Admin grants full permissions in both Chat and the Ops Center."
        >
          Admin
        </span>
        <Segmented
          value={role === "admin" ? "admin" : ""}
          options={ADMIN_OPTIONS}
          onChange={() => onChange({ role: "admin", opsRole: "admin" })}
          label={`${ariaPrefix}Admin permissions`}
        />
      </div>
      <div className="grid justify-items-start gap-[0.3rem]">
        <span
          className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]"
          title="Chat permissions: what this account can do in chat. Viewer is read-only."
        >
          Chat
        </span>
        <Segmented
          value={role}
          options={CHAT_ROLE_OPTIONS}
          onChange={(nextRole) =>
            onChange({
              role: nextRole,
              // Leaving unified Admin revokes its implied Ops Admin grant.
              opsRole: role === "admin" ? "none" : opsRole,
            })
          }
          label={`${ariaPrefix}Chat permissions`}
          dividers
        />
      </div>
      <div className="grid justify-items-start gap-[0.3rem]">
        <span
          className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]"
          title="Ops Center permissions: Viewer sees tasks and logs; Contributor also creates tasks."
        >
          Ops Center
        </span>
        <Segmented
          value={opsRole}
          options={OPS_ROLE_OPTIONS}
          onChange={(nextOpsRole) =>
            onChange({
              // The member API role is the Chat Contributor fallback when a
              // narrower Ops role replaces unified Admin.
              role: role === "admin" ? "member" : role,
              opsRole: nextOpsRole,
            })
          }
          label={`${ariaPrefix}Ops Center permissions`}
          dividers
        />
      </div>
    </>
  );
}

// TeamPicker replaces the free-text team field (Item A5). Typing a team name
// silently created a new one on any typo or case difference — "Testing" and
// "testing" became two trust groups, and the difference only shows up later,
// as a project a teammate cannot see. Assignment is now a choice from the
// teams that exist, with "New team…" as the deliberate way to make one.
//
// The list comes from the accounts on this page, so a team whose only holder
// is a team-shared project (its members all left) is not offered — "New team…"
// still types that name if an admin needs it, and the server's own guards
// apply either way.
function TeamPicker({
  label,
  teams,
  value,
  onChange,
  className,
}: {
  label: string;
  teams: string[];
  value: string;
  onChange: (team: string) => void;
  className?: string;
}) {
  // Creating a new team is a mode, not a value: the select shows "New team…"
  // while the adjacent input holds the draft. It opens automatically when the
  // current value is a name no account holds yet (a fresh team mid-edit).
  //
  // RESYNCED when `value` changes, not derived once. The kebab popover this
  // renders in is reused across rows, so choosing "New team…" for one account
  // and then opening another account's kebab left the mode set: the second
  // account's real team appeared inside a "New team name" box while the select
  // claimed a new team was being created.
  const known = value === "" || teams.includes(value);
  const [creating, setCreating] = useState(!known);
  const [seenValue, setSeenValue] = useState(value);
  if (value !== seenValue) {
    setSeenValue(value);
    setCreating(!(value === "" || teams.includes(value)));
  }
  const showInput = creating || !known;
  return (
    <span className="flex min-w-0 flex-1 items-center gap-[0.35rem]">
      <select
        aria-label={label}
        value={showInput ? "__new__" : value}
        onChange={(e) => {
          if (e.target.value === "__new__") {
            setCreating(true);
            onChange("");
            return;
          }
          setCreating(false);
          onChange(e.target.value);
        }}
        className={className}
      >
        <option value="">— No team</option>
        {teams.map((t) => (
          <option key={t} value={t}>
            {t}
          </option>
        ))}
        <option value="__new__">New team…</option>
      </select>
      {showInput ? (
        <input
          aria-label={`${label}: new team name`}
          placeholder="Team name"
          maxLength={64}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className={className}
        />
      ) : null}
    </span>
  );
}

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
// Wrap-safety for anything server-authored that lands inside a table cell.
// Table layout sizes a column to its content, so one long unbroken token (a
// server sentence, a project name, a generated password, a long email) both
// refuses to wrap AND widens the table until the whole thing grows a
// horizontal scrollbar. `overflow-wrap: anywhere` breaks the token, and the
// max-width caps what the cell can contribute to the column's preferred width.
const TD_TEXT_WRAP =
  "block max-w-[26rem] whitespace-normal [overflow-wrap:anywhere]";
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

const MENU_WIDTH_PX = 352; // 22rem — the three permission sections need room
const MENU_EST_HEIGHT_PX = 375; // flip-above threshold (three permission sections + team)

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
  // Delete is the one mutation whose failure is REPORTED IN THE PANEL rather
  // than in the row (QA #21). The fail-closed 409 for an account that still
  // owns team-shared projects is a paragraph naming projects to transfer — a
  // next step, decided right where Confirm delete was clicked — and rendering
  // it in the row put it far from the decision and widened the whole table.
  // So the panel stays open on failure and holds the refusal; only a
  // SUCCESSFUL delete closes it.
  const [deleteError, setDeleteError] = useState<{
    email: string;
    message: string;
    // The projects the fail-closed guard named, WITH their ids, so each can
    // link at its own transfer control. Undefined for any other failure, and
    // for a server predating the structured 409 body.
    projects?: OwnedSharedProject[];
  } | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);
  // ── discovery toolbar state (search / filters / grouping) ──
  const [query, setQuery] = useState("");
  const [filterRole, setFilterRole] = useState("all");
  const [filterOps, setFilterOps] = useState("all");
  const [filterTeam, setFilterTeam] = useState("all");
  // Rename-team flow: non-null = the inline rename input is open, holding the
  // draft new name. Renames relabel the team everywhere (all members + team-
  // shared projects) server-side in one transaction.
  const [renameDraft, setRenameDraft] = useState<string | null>(null);
  const [renameStatus, setRenameStatus] = useState<string | null>(null);
  // "Rename team" swaps the button for an input; focus follows to it so the
  // flow is usable without a mouse. An explicit focus() (not autoFocus) ties
  // the move to the moment the user opened the rename — a state change a
  // screen reader announces — instead of to React mounting the node.
  const renameInputRef = useRef<HTMLInputElement | null>(null);
  const renameOpen = renameDraft !== null;
  useEffect(() => {
    if (renameOpen) renameInputRef.current?.focus();
  }, [renameOpen]);
  // The row-edit popover: focus enters it on open (it renders after the whole
  // table in the DOM, so without this a keyboard user would have to tab past
  // every remaining row to reach the controls they just opened), and the
  // outside-click close below tests containment against this ref rather than
  // having the popover swallow clicks with an onClick on a plain <div>.
  const popRef = useRef<HTMLDivElement | null>(null);
  // The kebab that opened the popover, so focus can be handed back when it
  // closes rather than dropped on the document.
  const menuTriggerRef = useRef<HTMLButtonElement | null>(null);
  const menuEmail = menu?.email ?? null;
  useEffect(() => {
    if (menuEmail) {
      popRef.current?.focus();
      return;
    }
    menuTriggerRef.current?.focus();
    menuTriggerRef.current = null;
  }, [menuEmail]);

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
  // document-level listeners). "Outside" is decided here, by containment
  // against the popover element — the popover itself no longer carries a
  // stopPropagation onClick handler it had no accessible role for.
  useEffect(() => {
    if (!menu) return;
    // A delete in flight pins the panel open: it is the only surface that
    // reports the refusal, so dismissing it mid-request would drop the
    // server's answer on the floor.
    if (deleting) return;
    const close = (e: globalThis.MouseEvent) => {
      const target = e.target as Node | null;
      if (target && popRef.current?.contains(target)) return;
      // Returning focus to the kebab is right for Escape, for an action taken
      // inside the popover, and for a click on dead space — in each case the
      // user dismissed the popover without saying where focus should go, and
      // dropping it to <body> would lose a keyboard user's place. It is WRONG
      // when the click landed on another CONTROL: that click already chose the
      // focus target, and pulling it back to the kebab yanks focus out of the
      // thing the user just clicked. So the restore is skipped only in that
      // case, by dropping the stored trigger before the close.
      if (
        target instanceof Element &&
        target.closest(
          'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
        )
      ) {
        menuTriggerRef.current = null;
      }
      setMenu(null);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenu(null);
    };
    document.addEventListener("click", close);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("click", close);
      document.removeEventListener("keydown", onKey);
    };
  }, [menu, deleting]);

  const openMenu = (u: AdminUser, e: MouseEvent<HTMLButtonElement>) => {
    e.stopPropagation();
    // A previous refusal belongs to the panel session that provoked it.
    setDeleteError(null);
    menuTriggerRef.current = e.currentTarget;
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
    // Send ONLY what changed. Resending an unchanged role used to make a
    // self-edit look like a self-demotion upstream — an ADMIN_EMAILS bootstrap
    // admin (whose users.role is the default "member") could not set their own
    // team at all (#1157). The server-side guard is narrowed too; this keeps
    // the request honest about what the admin actually touched.
    //
    // ops_role rides along on the same rule: an explicit write pins an
    // ops-plane row, and untouched accounts should keep following the implied
    // chat-admin semantics.
    const body: Record<string, string> = {};
    if (next.role !== u.role) body.role = next.role;
    if (next.team_id !== u.team_id) body.team_id = next.team_id;
    if (next.ops_role !== undefined && next.ops_role !== opsRoleOf(u)) {
      body.ops_role = next.ops_role;
    }
    if (Object.keys(body).length === 0) return; // nothing to do
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
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

  // The panel is left OPEN while this runs and on failure: the refusal belongs
  // under the button that provoked it. Success removes the row and closes it.
  const remove = async (u: AdminUser) => {
    setDeleteError(null);
    setDeleting(u.email);
    try {
      const res = await fetch(
        `/api/admin/users/${encodeURIComponent(u.email)}`,
        { method: "DELETE" },
      );
      if (!res.ok) {
        const raw = (await res.text()).trim();
        // The fail-closed refusal is JSON carrying {id, name} per project, so
        // the notice can link at each transfer control. A pre-structured
        // server answers in plain text; that still renders, with names parsed
        // out of the prose and no links.
        let message = "";
        let projects: OwnedSharedProject[] | undefined;
        try {
          const body = JSON.parse(raw) as {
            error?: string;
            owns_shared_projects?: OwnedSharedProject[];
          };
          if (typeof body.error === "string") message = body.error.trim();
          if (Array.isArray(body.owns_shared_projects)) {
            const named = body.owns_shared_projects.filter(
              (p) => p && typeof p.id === "string" && typeof p.name === "string",
            );
            if (named.length > 0) projects = named;
          }
        } catch {
          message = raw;
        }
        // readErrorText's 200-char rule (an anti-HTML-error-page guard) is too
        // tight for THIS body: the refusal grows one project name at a time,
        // and an account owning a handful of them would have had its whole
        // explanation replaced by "Delete failed (409)." — the exact sentence
        // the admin needs. So it gets a larger, still bounded allowance, and
        // only when the body actually is that refusal.
        const isRefusal =
          Boolean(projects) || parseOwnedSharedProjects(message).length > 0;
        setDeleteError({
          email: u.email,
          message:
            message.length > 0 && message.length <= (isRefusal ? 1000 : 200)
              ? message
              : `Delete failed (${res.status}).`,
          projects,
        });
        return;
      }
      setUsers((prev) =>
        prev ? prev.filter((x) => x.email !== u.email) : prev,
      );
      setMenu(null);
    } catch (err) {
      setDeleteError({
        email: u.email,
        message: err instanceof Error ? err.message : "Delete failed.",
      });
    } finally {
      setDeleting(null);
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
  const [newOpsRole, setNewOpsRole] = useState<OpsRole>("none");
  const [newTeam, setNewTeam] = useState("");
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
          ops_role: newOpsRole,
          team_id: newTeam.trim(),
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
      setNewOpsRole("none");
      setNewTeam("");
      setAddStatus(null);
      setAddOpen(false);
    } catch (err) {
      setAddStatus(err instanceof Error ? err.message : "Create failed.");
    }
  };

  const renameTeam = async (from: string, to: string) => {
    setRenameStatus("saving");
    try {
      const res = await fetch("/api/admin/teams/rename", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ from, to }),
      });
      if (!res.ok) {
        throw new Error(
          await readErrorText(res, `Rename failed (${res.status}).`),
        );
      }
      // The server relabels team-shared PROJECTS as well as members and
      // reports both counts; saying only the member count hid the half of the
      // rename with the wider blast radius.
      const out = (await res.json()) as {
        users_updated?: number;
        projects_updated?: number;
      };
      const members = out.users_updated ?? 0;
      const projects = out.projects_updated ?? 0;
      setRenameStatus(
        `Renamed "${from}" to "${to.trim()}" (${members} member${members === 1 ? "" : "s"}, ${projects} shared project${projects === 1 ? "" : "s"}).`,
      );
      setRenameDraft(null);
      setFilterTeam(to.trim());
      const rows = await fetchUsers();
      if (rows !== null) setUsers(rows);
    } catch (err) {
      setRenameStatus(err instanceof Error ? err.message : "Rename failed.");
    }
  };

  const addDisabled =
    addStatus === "saving" || newEmail.trim() === "" || newPassword.length < 8;

  if (admin !== "admin") return <AdminGateFallback state={admin} />;

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
          the{" "}
          <Link href="/orchestrator?tab=usage">Operations Center → Usage</Link>{" "}
          and <Link href="/orchestrator?tab=adoption">Adoption</Link> views.
        </>
      }
    >
      {error ? (
        <NoticeBanner tone="danger">{error}</NoticeBanner>
      ) : (
        <ConnPanel>
          {/* ── discovery toolbar: search on its own bar, filters sharing one
              row as equal thirds (SETTINGS_INPUT carries w-full, so the
              selects need the important override to sit side by side). ── */}
          <div className="mb-3 grid gap-[0.55rem]">
            <input
              aria-label="Search users"
              placeholder="Search email or team…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              className={SETTINGS_INPUT}
            />
            <div className="flex flex-wrap items-center gap-[0.55rem]">
              <select
                aria-label="Filter by chat role"
                value={filterRole}
                onChange={(e) => setFilterRole(e.target.value)}
                className={`${SETTINGS_INPUT} w-auto! min-w-[7.5rem] flex-1 basis-0`}
              >
                <option value="all">Chat: all</option>
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    Chat: {CHAT_ROLE_LABELS[r].toLowerCase()}
                  </option>
                ))}
              </select>
              <select
                aria-label="Filter by Ops Center role"
                value={filterOps}
                onChange={(e) => setFilterOps(e.target.value)}
                className={`${SETTINGS_INPUT} w-auto! min-w-[7.5rem] flex-1 basis-0`}
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
                className={`${SETTINGS_INPUT} w-auto! min-w-[7.5rem] flex-1 basis-0`}
              >
                <option value="all">Team: all</option>
                <option value="(none)">Team: none</option>
                {teams.map((t) => (
                  <option key={t} value={t}>
                    Team: {t}
                  </option>
                ))}
              </select>
              {filterTeam !== "all" && filterTeam !== "(none)" ? (
                renameDraft === null ? (
                  <button
                    type="button"
                    onClick={() => {
                      setRenameStatus(null);
                      setRenameDraft(filterTeam);
                    }}
                    className={btnClass({ sm: true, reveal: true })}
                    title="Rename this team for every member and every team-shared project"
                  >
                    Rename team
                  </button>
                ) : (
                  <span className="inline-flex items-center gap-[0.35rem]">
                    <input
                      ref={renameInputRef}
                      aria-label={`New name for team ${filterTeam}`}
                      value={renameDraft}
                      onChange={(e) => setRenameDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (
                          e.key === "Enter" &&
                          renameDraft.trim() &&
                          renameDraft.trim() !== filterTeam
                        ) {
                          void renameTeam(filterTeam, renameDraft);
                        }
                        if (e.key === "Escape") setRenameDraft(null);
                      }}
                      className={`${SETTINGS_INPUT} w-[10rem]`}
                    />
                    <button
                      type="button"
                      disabled={
                        !renameDraft.trim() ||
                        renameDraft.trim() === filterTeam ||
                        renameStatus === "saving"
                      }
                      onClick={() => void renameTeam(filterTeam, renameDraft)}
                      className={btnClass({ variant: "primary", sm: true })}
                    >
                      {renameStatus === "saving" ? "Renaming…" : "Rename"}
                    </button>
                    <button
                      type="button"
                      onClick={() => setRenameDraft(null)}
                      className={btnClass({ sm: true, reveal: true })}
                    >
                      Cancel
                    </button>
                  </span>
                )
              ) : null}
            </div>
          </div>
          {renameStatus && renameStatus !== "saving" ? (
            <p className="mb-2 text-[0.72rem] text-[var(--color-text-secondary)]">
              {renameStatus}
            </p>
          ) : null}
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
                  {/* The kebab column. A blank header leaves the row-edit
                      buttons announced with no column name at all, so the
                      name is given visually-hidden rather than dropped. */}
                  <th className={`${TH} w-[2.4rem] text-right`}>
                    <span className="sr-only">Actions</span>
                  </th>
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
                            <span
                              className={`text-[0.8rem] text-[var(--color-text-primary)] ${TD_TEXT_WRAP}`}
                            >
                              {row.email}
                            </span>
                            {account?.role === "admin" ? (
                              <RoleBadge accent>Admin</RoleBadge>
                            ) : null}
                            {account?.role === "viewer" ? (
                              <RoleBadge>Viewer</RoleBadge>
                            ) : null}
                            {account &&
                            opsRoleOf(account) !== "none" &&
                            // A unified admin already has the accent Admin badge;
                            // repeating "ops: admin" would present one grant as two.
                            !(
                              account.role === "admin" &&
                              opsRoleOf(account) === "admin"
                            ) ? (
                              <RoleBadge
                                title={`Operations Center ${
                                  {
                                    admin: "admin",
                                    client: "contributor (creates tasks)",
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
                                    client: "contributor",
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
                            /* Shown ONCE, so it needs both halves of that
                               contract: a way to take the value (16 random
                               characters are not retyped from a table cell by
                               hand) and a way to put it away again, since an
                               admin screen is often shared. Without a dismiss
                               the credential simply sat here for the rest of
                               the session. */
                            <span
                              className={`mt-[0.18rem] flex flex-wrap items-center gap-[0.3rem] text-[0.7rem] text-[var(--color-text-muted)] ${TD_TEXT_WRAP}`}
                            >
                              New password (shown once):{" "}
                              <code className="font-[family-name:var(--font-code)] text-[var(--color-text-secondary)]">
                                {resetShown[row.email]}
                              </code>
                              <CopyButton
                                text={resetShown[row.email]}
                                title="Copy the new password"
                                variant="compact"
                              />
                              <button
                                type="button"
                                className="touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                                aria-label={`Hide the new password for ${row.email}`}
                                title="Hide"
                                onClick={() =>
                                  setResetShown((prev) => {
                                    const next = { ...prev };
                                    delete next[row.email];
                                    return next;
                                  })
                                }
                              >
                                ×
                              </button>
                            </span>
                          ) : null}
                          {/* Row status = the mutations that finish AFTER the
                              panel closes (role/team save, password reset).
                              Delete does not report here: its refusal is a
                              next-step paragraph and belongs under the button
                              that provoked it (see deleteError). Whatever the
                              server says still has to survive a table cell, so
                              it wraps and is width-capped. */}
                          {status ? (
                            <span
                              className={`mt-[0.18rem] text-[0.75rem] ${TD_TEXT_WRAP} ${
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
              ref={popRef}
              tabIndex={-1}
              role="dialog"
              aria-label={`Edit ${menu.email}`}
              style={{ left: menu.x, top: menu.y }}
              // max-h + scroll: the panel is fixed-positioned from an
              // estimated height, and the delete refusal is the one child that
              // can grow it by several lines — without this it could run off
              // the bottom of the viewport with no way to reach it.
              className="fixed z-[400] grid max-h-[calc(100vh-1rem)] w-[22rem] gap-[0.7rem] overflow-y-auto rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] px-[0.85rem] py-[0.8rem] shadow-[var(--shadow-md)] focus:outline-none motion-safe:animate-set-fade"
            >
              <div className="text-[0.76rem] font-semibold text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
                {menu.email}
              </div>
              <PermissionFields
                role={menu.role}
                opsRole={menu.opsRole}
                onChange={({ role, opsRole }) =>
                  setMenu({ ...menu, role, opsRole })
                }
              />
              <div className="grid gap-[0.3rem]">
                <span className="text-[0.64rem] font-bold uppercase tracking-[0.07em] text-[var(--color-text-muted)]">
                  Team
                </span>
                <TeamPicker
                  label={`Team for ${menu.email}`}
                  teams={teams}
                  value={menu.team}
                  onChange={(team) => setMenu({ ...menu, team })}
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
                {/* Rotating a password locks the user out of their current
                    one the moment it lands, so it arms first like Delete
                    beside it instead of firing off a single click. */}
                <InlineConfirmButton
                  label="Reset password"
                  confirmLabel="Confirm reset"
                  onConfirm={() => {
                    setMenu(null);
                    void resetPassword(menuAccount);
                  }}
                />
                <InlineConfirmButton
                  label="Delete user"
                  disabled={deleting !== null}
                  onConfirm={() => void remove(menuAccount)}
                />
              </div>
              {deleting === menu.email ? (
                <p className="text-[0.7rem] text-[var(--color-text-muted)]">
                  Deleting…
                </p>
              ) : null}
              {/* The refusal, where the action was taken. */}
              {deleteError && deleteError.email === menu.email ? (
                <DeleteRefusal
                  message={deleteError.message}
                  projects={deleteError.projects}
                  currentOwner={deleteError.email}
                  onTransferred={() => {
                    // The refusal's whole purpose is unblocking this delete,
                    // so the handover retries it rather than leaving the admin
                    // to re-open the kebab and click Confirm delete again. A
                    // second blocked project simply refuses again, naming what
                    // is left.
                    void remove(menuAccount);
                  }}
                />
              ) : null}
              {/* Deleting an account is a cascade, not a de-provision, and the
                  two-click button alone said none of it. The one thing it
                  CANNOT take is a team-shared project — the server refuses and
                  names what to transfer — so say that here too, where the
                  admin decides. */}
              <p className="mt-2 text-[0.7rem] leading-[1.5] text-[var(--color-text-muted)]">
                Deleting removes their chats, memories, connected MCP servers
                (and stored credentials), connector settings, skills and
                personal projects. Projects they share with a team are kept —
                transfer those first, from the project&rsquo;s settings.
              </p>
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
                <div className="grid w-full gap-[0.7rem] rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] p-[0.7rem]">
                  <PermissionFields
                    role={newRole}
                    opsRole={newOpsRole}
                    labelPrefix="New user"
                    onChange={({ role, opsRole }) => {
                      setNewRole(role);
                      setNewOpsRole(opsRole);
                    }}
                  />
                </div>
                <ConnField label="Team" grow>
                  <TeamPicker
                    label="New user team"
                    teams={teams}
                    value={newTeam}
                    onChange={setNewTeam}
                    className={SETTINGS_INPUT}
                  />
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
              Chat and Ops Center permissions are separate: chat Viewer is
              read-only, while Contributor can actively use Chat or create Ops
              Center tasks. Admin is one unified grant across both. CLI
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
