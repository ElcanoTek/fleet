"use client";

import { useEffect, useState } from "react";

import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

// The Users panel (#237) is the admin surface for the RBAC layer: it lists
// every provisioned account with its role + team and lets an admin manage them
// end-to-end — create accounts, reassign role/team, reset passwords, and
// delete — so user management no longer requires CLI access to the box
// (`fleet admin add` / `fleet chat user …` remain the scriptable equivalents).
// Roles gate access on the chat server (viewer = read-only, admin = full +
// this page); granting admin also grants the Operations Center admin row
// server-side (the same two-plane semantics as `fleet admin add`), surfaced
// here via the ops_center_admin annotation.

export type AdminUser = {
  email: string;
  role: string;
  team_id: string;
  created_at: number;
  updated_at: number;
  ops_center_admin?: boolean;
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

export function UsersPanel() {
  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Per-email status for inline feedback ("saving" | "saved" | error text).
  const [rowStatus, setRowStatus] = useState<Record<string, string>>({});
  // Per-email freshly reset password, shown ONCE until dismissed/navigated.
  const [resetShown, setResetShown] = useState<Record<string, string>>({});
  // Two-click delete: first click arms, second confirms.
  const [deleteArmed, setDeleteArmed] = useState<string | null>(null);

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
        throw new Error(await readErrorText(res, `Save failed (${res.status}).`));
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

  const remove = async (u: AdminUser) => {
    setDeleteArmed(null);
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      const res = await fetch(`/api/admin/users/${encodeURIComponent(u.email)}`, { method: "DELETE" });
      if (!res.ok) {
        throw new Error(await readErrorText(res, `Delete failed (${res.status}).`));
      }
      setUsers((prev) => (prev ? prev.filter((x) => x.email !== u.email) : prev));
    } catch (err) {
      setRowStatus((s) => ({ ...s, [u.email]: err instanceof Error ? err.message : "Delete failed." }));
    }
  };

  const resetPassword = async (u: AdminUser) => {
    const password = generatePassword();
    setRowStatus((s) => ({ ...s, [u.email]: "saving" }));
    try {
      const res = await fetch(`/api/admin/users/${encodeURIComponent(u.email)}/password`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });
      if (!res.ok) {
        throw new Error(await readErrorText(res, `Reset failed (${res.status}).`));
      }
      setRowStatus((s) => ({ ...s, [u.email]: "saved" }));
      setResetShown((s) => ({ ...s, [u.email]: password }));
    } catch (err) {
      setRowStatus((s) => ({ ...s, [u.email]: err instanceof Error ? err.message : "Reset failed." }));
    }
  };

  // ── add-user form state ──
  const [newEmail, setNewEmail] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [newRole, setNewRole] = useState<Role>("member");
  const [addStatus, setAddStatus] = useState<string | null>(null);
  const [addedPassword, setAddedPassword] = useState<{ email: string; password: string } | null>(null);

  const addUser = async () => {
    setAddStatus("saving");
    setAddedPassword(null);
    try {
      const res = await fetch("/api/admin/users", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email: newEmail.trim(), password: newPassword, role: newRole }),
      });
      if (!res.ok) {
        throw new Error(await readErrorText(res, `Create failed (${res.status}).`));
      }
      const created = (await res.json()) as AdminUser;
      setUsers((prev) => (prev ? [...prev, created].sort((a, b) => a.email.localeCompare(b.email)) : [created]));
      setAddedPassword({ email: created.email, password: newPassword });
      setNewEmail("");
      setNewPassword("");
      setNewRole("member");
      setAddStatus(null);
    } catch (err) {
      setAddStatus(err instanceof Error ? err.message : "Create failed.");
    }
  };

  const addDisabled = addStatus === "saving" || newEmail.trim() === "" || newPassword.length < 8;

  return (
    <section className="mt-6">
      <h2 className="mb-2 text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">Users &amp; roles</h2>
      {error ? (
        <NoticeBanner tone="danger">{error}</NoticeBanner>
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
                          {u.ops_center_admin ? (
                            <span
                              title="Also an Operations Center admin"
                              className="inline-block rounded-full bg-[var(--color-overlay-soft)] px-2 py-0.5 text-[0.6875rem] font-medium uppercase tracking-wide text-[var(--color-text-muted)]"
                            >
                              ops
                            </span>
                          ) : null}
                        </div>
                        {resetShown[u.email] ? (
                          <div className="mt-1 text-[0.75rem] text-[var(--color-text-muted)]">
                            New password (shown once):{" "}
                            <code className="text-[var(--color-text-secondary)]">{resetShown[u.email]}</code>
                          </div>
                        ) : null}
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
                              className={`max-w-56 truncate text-[0.75rem] ${status === "saved" ? "text-[var(--color-text-muted)]" : "text-[var(--color-danger-soft)]"}`}
                              title={status}
                            >
                              {status === "saved" ? "Saved" : status}
                            </span>
                          ) : null}
                          <button
                            type="button"
                            onClick={() => save(u)}
                            disabled={!dirty(u, edits) || status === "saving"}
                            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            {status === "saving" ? "Saving…" : "Save"}
                          </button>
                          <button
                            type="button"
                            onClick={() => resetPassword(u)}
                            disabled={status === "saving"}
                            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            Reset password
                          </button>
                          <button
                            type="button"
                            onClick={() => (deleteArmed === u.email ? remove(u) : setDeleteArmed(u.email))}
                            onBlur={() => setDeleteArmed((cur) => (cur === u.email ? null : cur))}
                            disabled={status === "saving"}
                            className={`rounded-full border px-3 py-1 text-[0.8125rem] transition disabled:cursor-not-allowed disabled:opacity-40 ${
                              deleteArmed === u.email
                                ? "border-[var(--color-danger-soft)] text-[var(--color-danger-soft)]"
                                : "border-[var(--color-border-strong)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                            }`}
                          >
                            {deleteArmed === u.email ? "Confirm delete" : "Delete"}
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })
              ) : (
                <tr>
                  <td colSpan={4} className="px-4 py-4 text-center text-[var(--color-text-muted)]">
                    No users provisioned yet — add one below.
                  </td>
                </tr>
              )}
            </tbody>
          </table>

          {/* ── add user ── */}
          <div className="border-t border-[var(--color-border)] px-4 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <input
                aria-label="New user email"
                type="email"
                value={newEmail}
                placeholder="email@example.com"
                onChange={(ev) => setNewEmail(ev.target.value)}
                className="w-56 rounded-lg border border-[var(--color-border)] bg-transparent px-2 py-1 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
              <input
                aria-label="New user password"
                type="text"
                value={newPassword}
                placeholder="password (min 8 chars)"
                onChange={(ev) => setNewPassword(ev.target.value)}
                className="w-52 rounded-lg border border-[var(--color-border)] bg-transparent px-2 py-1 font-mono text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
              <button
                type="button"
                onClick={() => setNewPassword(generatePassword())}
                className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
              >
                Generate
              </button>
              <select
                aria-label="New user role"
                value={newRole}
                onChange={(ev) => setNewRole(ev.target.value as Role)}
                className="rounded-lg border border-[var(--color-border)] bg-transparent px-2 py-1 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              >
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
              <button
                type="button"
                onClick={addUser}
                disabled={addDisabled}
                className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.8125rem] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:cursor-not-allowed disabled:opacity-40"
              >
                {addStatus === "saving" ? "Adding…" : "Add user"}
              </button>
              {addStatus && addStatus !== "saving" ? (
                <span className="text-[0.75rem] text-[var(--color-danger-soft)]">{addStatus}</span>
              ) : null}
            </div>
            {addedPassword ? (
              <div className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
                Created <span className="text-[var(--color-text-secondary)]">{addedPassword.email}</span> — password
                (shown once): <code className="text-[var(--color-text-secondary)]">{addedPassword.password}</code>
              </div>
            ) : null}
            <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
              Granting <span className="uppercase">admin</span> also grants Operations Center admin; demoting or
              deleting revokes it. CLI equivalent: <code>fleet admin add</code>.
            </p>
          </div>
        </div>
      )}
    </section>
  );
}

function dirty(u: AdminUser, edits: Record<string, { role?: string; team_id?: string }>): boolean {
  const e = edits[u.email];
  if (!e) return false;
  return (e.role !== undefined && e.role !== u.role) || (e.team_id !== undefined && e.team_id !== u.team_id);
}
