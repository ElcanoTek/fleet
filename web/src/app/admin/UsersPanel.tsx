"use client";

import { useEffect, useState } from "react";

// The Users panel (#237) is the admin surface for the RBAC layer: it lists
// every provisioned account with its role + team and lets an admin reassign
// them. Roles gate access on the chat server (viewer = read-only, admin = full
// + this page); team + the per-conversation owner opt-in drive the team-scoped
// read view. This panel only edits role/team — provisioning/passwords stay on
// the `fleet chat user` CLI.

export type AdminUser = {
  email: string;
  role: string;
  team_id: string;
  created_at: number;
  updated_at: number;
};

const ROLES = ["member", "viewer", "admin"] as const;
type Role = (typeof ROLES)[number];

function roleBadgeClass(role: string): string {
  // Admin = accent, viewer = muted/read-only, member = neutral. Colors come
  // from the shared token palette so the badge themes with the rest of the app.
  switch (role) {
    case "admin":
      return "bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]";
    case "viewer":
      return "bg-[var(--color-overlay-soft)] text-[var(--color-text-muted)]";
    default:
      return "bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]";
  }
}

export function RoleBadge({ role }: { role: string }) {
  return (
    <span
      className={`inline-block rounded-full px-2 py-0.5 text-[0.6875rem] font-medium uppercase tracking-wide ${roleBadgeClass(role)}`}
    >
      {role}
    </span>
  );
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

export function UsersPanel() {
  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Per-email status for inline save feedback ("saving" | "saved" | error text).
  const [rowStatus, setRowStatus] = useState<Record<string, string>>({});

  const apply = (isStale: () => boolean) => {
    fetchUsers()
      .then((rows) => {
        if (isStale() || rows === null) return;
        setUsers(rows);
      })
      .catch((err: unknown) => {
        if (isStale()) return;
        setError(err instanceof Error ? err.message : "Failed to load.");
      })
      .finally(() => {
        if (isStale()) return;
        setLoading(false);
      });
  };

  useEffect(() => {
    let stale = false;
    apply(() => stale);
    return () => {
      stale = true;
    };
  }, []);

  // Local edits before save. Keyed by email so a row's pending role/team are
  // independent of the others.
  const [edits, setEdits] = useState<Record<string, { role?: string; team_id?: string }>>({});

  const editFor = (u: AdminUser) => ({
    role: edits[u.email]?.role ?? u.role,
    team_id: edits[u.email]?.team_id ?? u.team_id,
  });

  const setEdit = (email: string, patch: { role?: string; team_id?: string }) => {
    setEdits((prev) => ({ ...prev, [email]: { ...prev[email], ...patch } }));
  };

  const save = async (u: AdminUser) => {
    const next = editFor(u);
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      const res = await fetch(`/api/admin/users/${encodeURIComponent(u.email)}`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ role: next.role, team_id: next.team_id }),
      });
      if (!res.ok) {
        const msg = res.status === 400 ? "Invalid role." : `Save failed (${res.status}).`;
        throw new Error(msg);
      }
      const updated = (await res.json()) as AdminUser;
      setUsers((prev) => (prev ? prev.map((x) => (x.email === u.email ? updated : x)) : prev));
      setEdits((prev) => {
        const { [u.email]: _drop, ...rest } = prev;
        return rest;
      });
      setRowStatus((s) => ({ ...s, [u.email]: "saved" }));
    } catch (err) {
      setRowStatus((s) => ({ ...s, [u.email]: err instanceof Error ? err.message : "Save failed." }));
    }
  };

  const dirty = (u: AdminUser) => {
    const e = edits[u.email];
    if (!e) return false;
    return (e.role !== undefined && e.role !== u.role) || (e.team_id !== undefined && e.team_id !== u.team_id);
  };

  return (
    <section className="mt-6">
      <h2 className="mb-2 text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">Users &amp; roles</h2>
      {error ? (
        <div className="rounded-[0.95rem] border border-[#e08080] bg-[color-mix(in_srgb,#e08080_15%,transparent)] px-4 py-3 text-[0.875rem] text-[#e08080]">
          {error}
        </div>
      ) : (
        <div className="overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
          <table className="w-full text-[0.875rem]">
            <thead className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              <tr className="border-b border-[var(--color-border)]">
                <th className="px-4 py-2 text-left">User</th>
                <th className="px-4 py-2 text-left">Role</th>
                <th className="px-4 py-2 text-left">Team</th>
                <th className="px-4 py-2 text-right">&nbsp;</th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                <tr>
                  <td colSpan={4} className="px-4 py-4 text-center text-[var(--color-text-muted)]">
                    Loading…
                  </td>
                </tr>
              ) : users && users.length > 0 ? (
                users.map((u) => {
                  const e = editFor(u);
                  const status = rowStatus[u.email];
                  return (
                    <tr key={u.email} className="border-b border-[var(--color-border-subtle)] last:border-none">
                      <td className="px-4 py-2 text-[var(--color-text-primary)]">
                        <div className="flex items-center gap-2">
                          <span>{u.email}</span>
                          <RoleBadge role={u.role} />
                        </div>
                      </td>
                      <td className="px-4 py-2">
                        <select
                          aria-label={`Role for ${u.email}`}
                          value={e.role as Role}
                          onChange={(ev) => setEdit(u.email, { role: ev.target.value })}
                          className="rounded-lg border border-[var(--color-border)] bg-transparent px-2 py-1 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                        >
                          {ROLES.map((r) => (
                            <option key={r} value={r}>
                              {r}
                            </option>
                          ))}
                        </select>
                      </td>
                      <td className="px-4 py-2">
                        <input
                          aria-label={`Team for ${u.email}`}
                          value={e.team_id}
                          placeholder="—"
                          onChange={(ev) => setEdit(u.email, { team_id: ev.target.value })}
                          className="w-32 rounded-lg border border-[var(--color-border)] bg-transparent px-2 py-1 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                        />
                      </td>
                      <td className="px-4 py-2 text-right">
                        <div className="flex items-center justify-end gap-2">
                          {status && status !== "saving" ? (
                            <span
                              className={`text-[0.75rem] ${status === "saved" ? "text-[var(--color-text-muted)]" : "text-[#e08080]"}`}
                            >
                              {status === "saved" ? "Saved" : status}
                            </span>
                          ) : null}
                          <button
                            type="button"
                            onClick={() => save(u)}
                            disabled={!dirty(u) || status === "saving"}
                            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            {status === "saving" ? "Saving…" : "Save"}
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })
              ) : (
                <tr>
                  <td colSpan={4} className="px-4 py-4 text-center text-[var(--color-text-muted)]">
                    No users provisioned yet — add one with{" "}
                    <code className="text-[var(--color-text-secondary)]">fleet chat user add</code>.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
